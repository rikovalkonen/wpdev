package cli

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type PullCfg struct {
	Exclude []string `yaml:"exclude,omitempty"`
}

func validatePullConfig(c *Config) error {
	for _, pattern := range c.Pull.Exclude {
		if pattern == "" || strings.ContainsAny(pattern, "\x00\r\n\\") || strings.Contains("/"+pattern+"/", "/../") {
			return fmt.Errorf("pull.exclude must contain nonempty rsync path patterns without .. or control characters")
		}
	}
	return nil
}

// Uploads are copied separately, after publication. Host infrastructure,
// runtime caches and production database/cache drop-ins stay out of the copy.
var pullFileExcludes = []string{
	".git", ".svn", ".hg", ".env", ".env.*", "/*.sql", "/*.sql.gz", "/*.log", "/wp-content/debug.log",
	"/.wpdev*", "/.gitignore", "/.ssh", "/docker-compose*", "/compose.yml", "/compose.yaml", "/Dockerfile",
	"/wp-cli.yml", "/wp-cli.local.yml",
	"/wp-config.php", "/.htaccess", "/.user.ini", "/php.ini",
	"/wp-content/uploads", "/wp-content/cache", "/wp-content/upgrade", "/wp-content/upgrade-temp-backup",
	"/wp-content/updraft", "/wp-content/ai1wm-backups", "/wp-content/backups",
	"/wp-content/object-cache.php", "/wp-content/advanced-cache.php", "/wp-content/db.php", "/wp-content/sunrise.php",
	"/wp-content/mu-plugins/wpdev-local.php",
}

type pullFile struct {
	rel, before, backup string
	mode                os.FileMode
	changed             bool
}
type pullFiles struct {
	dir       string
	files     []pullFile
	newConfig bool
}

func remoteRsync(c *Config, remote SSHCfg, source, dest string, excludes []string, out io.Writer) error {
	if err := remote.validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(source) || filepath.Clean(source) == "/" || strings.ContainsAny(source, "\x00\r\n") {
		return fmt.Errorf("invalid remote file path")
	}
	transport := append([]string{"ssh"}, remote.args()...)
	for i := range transport {
		transport[i] = shellQuote(transport[i])
	}
	// Retain symlinks only in staging so validation can reject them explicitly,
	// instead of following them into another installation or silently omitting files.
	args := []string{"-rlt", "-e", strings.Join(transport, " ")}
	for _, exclude := range excludes {
		args = append(args, "--exclude="+exclude)
	}
	args = append(args, "--", remote.destination()+":"+shellQuote(source), dest)
	return process(c.Root, nil, out, "rsync", args...)
}

func fetchRemoteFiles(c *Config, remote SSHCfg, meta remoteMetadata, dir string, out io.Writer) (*pullFiles, error) {
	if !filepath.IsAbs(meta.Root) || filepath.Clean(meta.Content) != filepath.Join(meta.Root, "wp-content") || filepath.Clean(meta.Plugins) != filepath.Join(meta.Root, "wp-content", "plugins") {
		return nil, fmt.Errorf("file pull requires the standard WordPress wp-content/plugins layout; use --db-only with prepared local files for custom layouts")
	}
	excludes := append([]string(nil), pullFileExcludes...)
	for _, p := range []string{c.ConfigPath, c.file(c.Database.DataPath)} {
		rel, err := filepath.Rel(c.file(c.Web.Docroot), p)
		if err == nil && safePullRelative(rel) {
			excludes = append(excludes, "/"+filepath.ToSlash(rel))
		}
	}
	if rel, err := filepath.Rel(meta.Root, meta.Uploads); err == nil && safePullRelative(rel) {
		excludes = append(excludes, "/"+filepath.ToSlash(rel))
	}
	state, err := readPluginState(c)
	if err != nil {
		return nil, err
	}
	for _, id := range sortedPluginIDs(state.Plugins) {
		if state.Plugins[id].Managed {
			excludes = append(excludes, "/wp-content/plugins/"+id)
		}
	}
	excludes = append(excludes, c.Pull.Exclude...)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fmt.Fprintln(out, "Downloading WordPress core, themes, plugins and site files (uploads use --uploads).")
	if err = remoteRsync(c, remote, strings.TrimRight(remote.WPPath, "/")+"/", dir+"/", excludes, out); err != nil {
		return nil, err
	}
	// Preserve an existing project's config. For an empty project, obtain the
	// source config (WordPress also allows it one directory above the core).
	config, _, err := configSource(c)
	if os.IsNotExist(err) {
		var path bytes.Buffer
		if err = remoteWP(c, remote, &path, "config", "path"); err != nil {
			return nil, err
		}
		if err = remoteRsync(c, remote, strings.TrimSpace(path.String()), filepath.Join(dir, "wp-config.php"), nil, out); err != nil {
			return nil, err
		}
		fmt.Fprintln(out, "Preparing source wp-config.php with conditional local database settings.")
	} else if err != nil {
		return nil, err
	} else if err = atomicWrite(filepath.Join(dir, "wp-config.php"), config, 0o600); err != nil {
		return nil, err
	}
	return planPullFiles(c, dir, out)
}

func safePullRelative(rel string) bool {
	return rel != "" && rel != "." && !filepath.IsAbs(rel) && filepath.Clean(rel) == rel && rel != ".." && !strings.HasPrefix(rel, "../") && !strings.ContainsAny(rel, "\x00\r\n\\")
}

func checkPullPath(c *Config, rel string) error {
	if !safePullRelative(rel) {
		return fmt.Errorf("invalid local file path %q", rel)
	}
	// Repeat critical rsync exclusions at publication boundaries, including
	// case variants on macOS filesystems. Never let a site file replace Git,
	// wpdev configuration or the local database's bind-mounted storage.
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, ".hg") || strings.EqualFold(part, ".svn") {
			return fmt.Errorf("file pull cannot replace repository metadata: %s", rel)
		}
	}
	protected := []string{c.file(".wpdev"), c.ConfigPath, c.file("docker-compose.yml"), c.file("docker-compose.override.yml"), c.file("docker-compose.override.yaml"), c.file(".gitignore")}
	if c.Database.Persist == "bind" {
		protected = append(protected, c.file(c.Database.DataPath))
	}
	for _, path := range protected {
		dest, guard := strings.ToLower(c.file(rel)), strings.ToLower(filepath.Clean(path))
		if dest == guard || strings.HasPrefix(dest, guard+string(os.PathSeparator)) {
			return fmt.Errorf("file pull cannot replace local configuration or database storage: %s", rel)
		}
	}
	for p := c.file(rel); p != c.Root; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("file pull would replace or follow local symlink %s; use pull.exclude or --db-only", p)
		}
		if p != c.file(rel) && !info.IsDir() {
			return fmt.Errorf("file pull parent is not a directory: %s", p)
		}
	}
	return nil
}

func pullFileFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("file pull requires a regular file: %s", path)
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func planPullFiles(c *Config, dir string, out io.Writer) (*pullFiles, error) {
	p := &pullFiles{dir: dir}
	for _, name := range []string{"wp-load.php", "wp-settings.php", "wp-includes/version.php", "wp-config.php"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("downloaded WordPress is missing a regular %s; check ssh.wp_path, exclusions and remote symlinks", name)
		}
	}
	if err := checkPullPath(c, filepath.Join(c.Web.Docroot, "wp-config.php")); err != nil {
		return nil, err
	}
	added, replaced := 0, 0
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("remote file %s is a symlink or special file; use pull.exclude or resolve the source file before pulling", name)
		}
		if name == "wp-config.php" {
			return nil // Config is transformed and published by the importer.
		}
		rel := filepath.Join(c.Web.Docroot, name)
		if err = checkPullPath(c, rel); err != nil {
			return err
		}
		before, err := pullFileFingerprint(c.file(rel))
		if err != nil {
			return err
		}
		after, err := pullFileFingerprint(path)
		if err != nil {
			return err
		}
		file := pullFile{rel: rel, before: before, backup: rel, changed: before != after, mode: 0o644 | info.Mode().Perm()&0o111}
		if file.changed {
			if before == "" {
				added++
				// Back up the first absent ancestor too, so rollback removes
				// directories created by a first pull into an empty project.
				for parent := filepath.Dir(rel); parent != "."; parent = filepath.Dir(parent) {
					if _, err := os.Lstat(c.file(parent)); !os.IsNotExist(err) {
						break
					}
					file.backup = parent
				}
			} else {
				replaced++
				fmt.Fprintln(out, "  Replace file:", filepath.ToSlash(rel))
			}
		}
		p.files = append(p.files, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "Files: %d new, %d replacements; additional local files are retained. Replacements will be backed up.\n", added, replaced)
	return p, nil
}

func (p *pullFiles) check(c *Config) error {
	for _, file := range p.files {
		if err := checkPullPath(c, file.rel); err != nil {
			return err
		}
		current, err := pullFileFingerprint(c.file(file.rel))
		if err != nil {
			return err
		}
		if current != file.before {
			return fmt.Errorf("local file changed during pull: %s; rerun to review the new changes", file.rel)
		}
	}
	return nil
}

func (p *pullFiles) snapshotPaths(c *Config) []string {
	var paths []string
	for _, file := range p.files {
		if file.changed {
			paths = append(paths, file.backup)
		}
	}
	sort.Strings(paths)
	var result []string
	for _, path := range paths {
		if len(result) == 0 || path != result[len(result)-1] && !strings.HasPrefix(path, result[len(result)-1]+string(os.PathSeparator)) {
			result = append(result, path)
		}
	}
	return result
}

func (p *pullFiles) publish(c *Config) error {
	if p.newConfig {
		ignore := c.file(".gitignore")
		b, err := os.ReadFile(ignore)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		line := "/" + filepath.ToSlash(filepath.Join(c.Web.Docroot, "wp-config.php"))
		if !strings.Contains("\n"+string(b)+"\n", "\n"+line+"\n") {
			if err = atomicWrite(ignore, append(b, []byte("\n# wpdev imported configuration\n"+line+"\n")...), 0o644); err != nil {
				return err
			}
		}
	}
	for _, file := range p.files {
		if !file.changed {
			continue
		}
		rel, err := filepath.Rel(c.Web.Docroot, file.rel)
		if err != nil {
			return err
		}
		if err = atomicCopy(filepath.Join(p.dir, rel), c.file(file.rel), file.mode); err != nil {
			return fmt.Errorf("publish %s: %w", file.rel, err)
		}
	}
	return nil
}
