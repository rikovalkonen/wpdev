package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var sshName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (s SSHCfg) validate() error {
	if !sshName.MatchString(s.Host) || (s.User != "" && !sshName.MatchString(s.User)) {
		return fmt.Errorf("SSH host/user must be plain names (SSH config aliases are supported)")
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("SSH port must be between 1 and 65535")
	}
	if !strings.HasPrefix(s.WPPath, "/") || strings.ContainsAny(s.WPPath, "\x00\r\n") || filepath.Clean(s.WPPath) == "/" {
		return fmt.Errorf("ssh.wp_path must be an absolute WordPress directory")
	}
	return nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func (s SSHCfg) destination() string {
	if s.User != "" {
		return s.User + "@" + s.Host
	}
	return s.Host
}
func (s SSHCfg) args() []string {
	a := []string{"-T", "-o", "BatchMode=yes"}
	if s.Port != 0 {
		a = append(a, "-p", strconv.Itoa(s.Port))
	}
	return a
}
func remoteWP(c *Config, remote SSHCfg, out io.Writer, args ...string) error {
	if err := remote.validate(); err != nil {
		return err
	}
	a := append([]string{"wp", "--allow-root", "--path=" + remote.WPPath, "--skip-plugins", "--skip-themes", "--skip-packages", "--no-color", "--quiet"}, args...)
	for i := range a {
		a[i] = shellQuote(a[i])
	}
	sshArgs := append(remote.args(), "--", remote.destination(), strings.Join(a, " "))
	return process(c.Root, nil, out, "ssh", sshArgs...)
}

type remoteMetadata struct {
	Prefix  string `json:"prefix"`
	Mode    string `json:"mode"`
	Uploads string `json:"uploads"`
	Root    string `json:"root"`
	Content string `json:"content"`
	Plugins string `json:"plugins"`
}

const remoteMetadataPHP = `echo json_encode(['prefix'=>$GLOBALS['wpdb']->base_prefix,'mode'=>is_multisite()?(is_subdomain_install()?'subdomain':'subdirectory'):'','uploads'=>wp_get_upload_dir()['basedir'],'root'=>rtrim(ABSPATH,'/'),'content'=>WP_CONTENT_DIR,'plugins'=>WP_PLUGIN_DIR]);`

func fetchRemote(c *Config, remote SSHCfg, dir string) (string, remoteMetadata, error) {
	var meta remoteMetadata
	var b bytes.Buffer
	if err := remoteWP(c, remote, &b, "eval", remoteMetadataPHP); err != nil {
		return "", meta, err
	}
	if err := json.Unmarshal(b.Bytes(), &meta); err != nil {
		return "", meta, fmt.Errorf("remote WordPress metadata: %w", err)
	}
	if !sqlIdentifier.MatchString(meta.Prefix) {
		return "", meta, fmt.Errorf("invalid remote table prefix")
	}
	// Select only this installation's tables; shared databases are common.
	b.Reset()
	if err := remoteWP(c, remote, &b, "db", "tables", "--all-tables-with-prefix", "--format=csv"); err != nil {
		return "", meta, err
	}
	tables := strings.Split(strings.TrimSpace(b.String()), ",")
	for _, table := range tables {
		if !sqlIdentifier.MatchString(table) || !strings.HasPrefix(table, meta.Prefix) {
			return "", meta, fmt.Errorf("invalid remote table list")
		}
	}
	tmp := filepath.Join(dir, "source.partial")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", meta, err
	}
	err = remoteWP(c, remote, f, "db", "export", "-", "--single-transaction", "--skip-lock-tables", "--tables="+strings.Join(tables, ","))
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(tmp)
		return "", meta, err
	}
	info, err := os.Stat(tmp)
	if err != nil {
		return "", meta, err
	}
	if info.Size() == 0 {
		os.Remove(tmp)
		return "", meta, fmt.Errorf("remote database export was empty")
	}
	dest := filepath.Join(dir, "source.sql")
	if err = os.Rename(tmp, dest); err != nil {
		return "", meta, err
	}
	return dest, meta, nil
}

func syncUploads(c *Config, remote SSHCfg, source string, out io.Writer) error {
	if err := remote.validate(); err != nil {
		return err
	}
	if !strings.HasPrefix(source, "/") || filepath.Clean(source) == "/" || strings.ContainsAny(source, "\x00\r\n") {
		return fmt.Errorf("invalid remote uploads path")
	}
	localPath, err := captureWP(c, "eval", `echo wp_get_upload_dir()['basedir'];`)
	if err != nil {
		return fmt.Errorf("inspect local uploads directory: %w", err)
	}
	relative, err := filepath.Rel("/var/www/html", strings.TrimSpace(localPath))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
		return fmt.Errorf("local uploads directory must be inside the mounted project")
	}
	dest := c.file(relative)
	// Do not follow a local symlink into another project.
	for p := dest; p != c.Root && p != filepath.Dir(p); p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("uploads destination contains a symlink: %s", p)
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	backup := c.file(".wpdev", "backups", "uploads-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return err
	}
	transport := []string{"ssh"}
	transport = append(transport, remote.args()...)
	for i := range transport {
		transport[i] = shellQuote(transport[i])
	}
	// Portable to macOS's rsync: quote the remote path explicitly. No --delete.
	args := []string{"-rlt", "--safe-links", "--backup", "--backup-dir=" + backup, "--itemize-changes",
		"-e", strings.Join(transport, " "), "--", remote.destination() + ":" + shellQuote(strings.TrimRight(source, "/")+"/"), dest + "/"}
	if err := process(c.Root, nil, out, "rsync", args...); err != nil {
		return fmt.Errorf("database was localized, but uploads are incomplete; rerun pull with --uploads-only (overwritten files: %s): %w", backup, err)
	}
	fmt.Fprintln(out, "Uploads synchronized; overwritten files backed up to", backup)
	return nil
}

var pullCmd = &cobra.Command{Use: "pull <environment>", Args: cobra.ExactArgs(1), Short: "Fetch WordPress files and localize the remote database"}

func init() {
	var opts importOptions
	var uploads, uploadsOnly, dbOnly bool
	addImportFlags(pullCmd, &opts)
	pullCmd.Flags().Lookup("yes").Usage = "Confirm replacement of local files and database"
	pullCmd.Flags().Lookup("dry-run").Usage = "Download, validate and preview file/database changes without publishing"
	pullCmd.Flags().BoolVar(&uploads, "uploads", false, "Also copy uploads with rsync, keeping extra local files")
	pullCmd.Flags().BoolVar(&uploadsOnly, "uploads-only", false, "Retry/copy uploads without replacing the database")
	pullCmd.Flags().BoolVar(&dbOnly, "db-only", false, "Skip code transfer and localize the database using existing WordPress files")
	pullCmd.MarkFlagsMutuallyExclusive("db-only", "uploads-only")
	pullCmd.RunE = func(cmd *cobra.Command, args []string) error {
		// Cobra's option storage is reused in tests and embedded callers.
		opts := opts
		c, err := projectConfig()
		if err != nil {
			return err
		}
		remote, ok := c.SSH[args[0]]
		if !ok {
			return fmt.Errorf("configure ssh.%s.host, user and wp_path first", args[0])
		}
		if err = remote.validate(); err != nil {
			return err
		}
		if uploadsOnly {
			if opts.DryRun {
				return fmt.Errorf("--uploads-only does not support --dry-run")
			}
			var out bytes.Buffer
			if err = remoteWP(c, remote, &out, "eval", remoteMetadataPHP); err != nil {
				return err
			}
			var meta remoteMetadata
			if err = json.Unmarshal(out.Bytes(), &meta); err != nil {
				return err
			}
			return syncUploads(c, remote, meta.Uploads, cmd.OutOrStdout())
		}
		dir, err := os.MkdirTemp("", "wpdev-pull-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		file, meta, err := fetchRemote(c, remote, dir)
		if err != nil {
			return err
		}
		if !dbOnly {
			opts.Files, err = fetchRemoteFiles(c, remote, meta, filepath.Join(dir, "wordpress"), cmd.OutOrStdout())
			if err != nil {
				return fmt.Errorf("file download failed; active project unchanged: %w", err)
			}
		}
		if opts.Prefix == "" {
			opts.Prefix = meta.Prefix
		}
		if opts.Mode == "" {
			opts.Mode = meta.Mode
		}
		if err = localizeDatabase(cmd, c, file, opts); err != nil {
			var pluginError *postImportPluginError
			if uploads && errors.As(err, &pluginError) {
				return fmt.Errorf("%w; uploads were not started: after fixing plugins, retry wpdev pull %s --uploads-only", err, args[0])
			}
			return err
		}
		if uploads && !opts.DryRun {
			return syncUploads(c, remote, meta.Uploads, cmd.OutOrStdout())
		}
		return nil
	}
}
