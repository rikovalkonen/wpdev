package cli

import (
	"context"
	"fmt"
	"github.com/spf13/cobra"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var doctorCmd = &cobra.Command{Use: "doctor", Short: "Check project configuration, DNS, certificates and HTTP clients", RunE: func(cmd *cobra.Command, args []string) error {
	c, err := projectConfig()
	if err != nil {
		return err
	}
	if err = loadAliases(c); err != nil {
		return err
	}
	failures := []string{}
	check := func(label string, err error) {
		if err != nil {
			failures = append(failures, label)
			fmt.Fprintf(cmd.OutOrStdout(), "FAIL %s: %v\n", label, err)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "OK  "+label)
		}
	}
	if c.TLS.Enabled {
		ca, err := os.ReadFile(c.file(".wpdev", "certs", "rootCA.pem"))
		if err == nil {
			err = verifyTLSFiles(c, ca)
		}
		check("TLS certificates and CA", err)
	}
	state, err := readTemplateState(c)
	check("Template state", err)
	if err == nil {
		for _, n := range templateNames {
			b, e := os.ReadFile(c.file(".wpdev", "templates", n))
			if e == nil && !managedTemplate(n, b, state) {
				fmt.Fprintln(cmd.OutOrStdout(), "NOTE custom template:", n, "— review native TLS/multisite integration with templates update")
			}
		}
	}
	for _, host := range c.Aliases {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		cancel()
		if err == nil {
			loopback := false
			for _, ip := range ips {
				if net.ParseIP(ip).IsLoopback() {
					loopback = true
				}
			}
			if !loopback {
				err = fmt.Errorf("expected a loopback address, got %v", ips)
			}
		}
		check("Host DNS "+host, err)
		_, err = captureCompose(c, "exec", "-T", "php", "getent", "hosts", host)
		check("Container DNS "+host, err)
	}
	url := c.URL()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, hostErr := client.Get(url)
	if hostErr == nil {
		response.Body.Close()
		if response.StatusCode >= 500 {
			hostErr = fmt.Errorf("HTTP %d", response.StatusCode)
		}
	}
	check("Host HTTP/TLS trust", hostErr)
	for _, transport := range []string{"curl", "stream"} {
		code := `$u=$argv[1];`
		if transport == "curl" {
			code += `$c=curl_init($u);curl_setopt_array($c,[CURLOPT_RETURNTRANSFER=>true,CURLOPT_TIMEOUT=>15]);$r=curl_exec($c);if($r===false){fwrite(STDERR,curl_error($c));exit(1);}echo curl_getinfo($c,CURLINFO_HTTP_CODE);`
		} else {
			code += `$r=file_get_contents($u,false,stream_context_create(['http'=>['timeout'=>15,'ignore_errors'=>true]]));if($r===false){exit(1);}echo 'connected';`
		}
		out, err := captureCompose(c, "exec", "-T", "php", "php", "-r", code, url)
		check("PHP "+transport+" "+url, err)
		if err == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "   "+strings.TrimSpace(out))
		}
	}
	if _, err := os.Stat(c.file(c.Web.Docroot, "wp-load.php")); err == nil {
		_, err = captureWP(c, "eval", `$r=wp_remote_get(`+phpLiteral(url)+`,['timeout'=>15]);if(is_wp_error($r)){WP_CLI::error($r->get_error_message());}echo wp_remote_retrieve_response_code($r);`)
		check("WordPress HTTP API", err)
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "SKIP WordPress HTTP API (WordPress files not present)")
	}
	if c.Multisite.Enabled {
		s, err := networkState(c)
		if err == nil {
			err = checkNetwork(c, s)
		}
		check("Multisite configuration", err)
	}
	if len(failures) > 0 {
		return fmt.Errorf("doctor found %d failed checks", len(failures))
	}
	return nil
}}
