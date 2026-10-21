package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDockerRemoteMediaAdapter(t *testing.T) {
	dockerTests(t)
	c := testConfig(t)
	m := mediaMap{MainID: 1, Sites: map[int]mediaSite{
		1: {Home: "https://source.example", SiteURL: "https://source.example/wp", Uploads: "https://source.example/wp/wp-content/uploads", LocalHome: "https://local.test"},
		2: {Home: "https://customer.example", SiteURL: "https://customer.example/wp", Uploads: "https://cdn.example/customer/uploads/sites/2", LocalHome: "https://local.test/child"},
	}}
	for _, tc := range []struct {
		name, env string
		active    bool
	}{{"local", "1", true}, {"outside", "0", true}, {"disabled", "1", false}} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"configured": true, "active": tc.active, "map": m, "overrides": RemoteMediaCfg{}})
			c.RemoteMediaJSON = string(b)
			helper, err := executeTemplate("remote-media.php.tmpl", bundled("remote-media.php.tmpl"), dataFor(c))
			if err != nil {
				t.Fatal(err)
			}
			script := `<?php
define('ABSPATH','/unused/');define('WP_HOME','unchanged-constant');
$site=1;$filters=[];
function get_current_blog_id(){return $GLOBALS['site'];}
function home_url(){return [1=>'https://local.test',2=>'https://local.test/child',3=>'https://later.local.test'][$GLOBALS['site']];}
function wp_get_upload_dir(){return ['baseurl'=>'https://local.test/wp-content/uploads'.($GLOBALS['site']>1?'/sites/'.$GLOBALS['site']:'')];}
function add_action(...$args){}
function add_filter($name,$callback,...$args){$GLOBALS['filters'][$name]=$callback;}
putenv('REMOTE_MEDIA_URL=original');putenv('WP_ENV=original');putenv('WP_HOME=original');
putenv('WPDEV_LOCAL=` + tc.env + `');
eval(substr(` + phpLiteral(string(helper)) + `,5));
if(WP_HOME!=='unchanged-constant')throw new RuntimeException('changed PHP constant');
`
			if tc.env != "1" {
				script += `if(getenv('REMOTE_MEDIA_URL')!=='original'||getenv('WP_HOME')!=='original'||getenv('WP_ENV')!=='original')throw new RuntimeException('outside-local side effects');`
			} else if !tc.active {
				script += `if(getenv('REMOTE_MEDIA_URL'))throw new RuntimeException('disabled remote media');`
			} else {
				script += `
$f=$filters['dude_remote_media_url'];
if(getenv('WP_HOME')!=='https://local.test'||getenv('REMOTE_MEDIA_URL')!=='https://source.example')throw new RuntimeException('wrong main environment');
if($f('https://source.example/wp-content/uploads/image.jpg?size=1')!=='https://source.example/wp/wp-content/uploads/image.jpg?size=1')throw new RuntimeException('source core path');
if($f('https://source.example/wp-content/uploads-other/image.jpg')!=='https://source.example/wp-content/uploads-other/image.jpg')throw new RuntimeException('missing prefix boundary');
$site=2;wpdev_media_environment();
if($f('https://local.test/wp-content/uploads/sites/2/image.jpg')!=='https://cdn.example/customer/uploads/sites/2/image.jpg')throw new RuntimeException('mapped CDN uploads');
$site=3;wpdev_media_environment();
if($f('https://local.test/wp-content/uploads/sites/3/image.jpg')!=='https://local.test/wp-content/uploads/sites/3/image.jpg')throw new RuntimeException('unmapped site changed');
$site=1;wpdev_media_environment();
if(getenv('REMOTE_MEDIA_URL')!=='https://source.example')throw new RuntimeException('restoration');
$wpdev_media['overrides']['url']='https://override.example/base';wpdev_media_environment();
if($f('https://override.example/base/wp-content/uploads/image.jpg')!=='https://override.example/base/wp/wp-content/uploads/image.jpg')throw new RuntimeException('manual override');
`
			}
			script += `echo 'adapter-ok';`
			var out bytes.Buffer
			if err = process(c.Root, strings.NewReader(script), &out, "docker", "run", "--rm", "-i", "--network=none", "php:8.5-cli", "php"); err != nil {
				t.Fatal(err)
			}
			if out.String() != "adapter-ok" {
				t.Fatal(out.String())
			}
		})
	}
}
