<?php
mysqli_report(MYSQLI_REPORT_ERROR | MYSQLI_REPORT_STRICT);
try {
    $request = json_decode(stream_get_contents(STDIN), true, 512, JSON_THROW_ON_ERROR);
    $db = new mysqli('db', 'root', 'root');
    $db->set_charset('utf8mb4');
    $quote = static fn($s) => '`' . str_replace('`', '``', $s) . '`';
    $schemas = [];
    foreach ($db->query('SHOW DATABASES')->fetch_all() as [$schema]) {
        if (in_array($schema, ['information_schema','performance_schema','mysql','sys'], true)) continue;
        $tables = $db->query('SHOW FULL TABLES FROM ' . $quote($schema))->fetch_all();
        if ($tables) $schemas[$schema] = $tables;
    }
    if (count($schemas) !== 1) throw new RuntimeException('Expected one nonempty application database in the dump');
    $schema = array_key_first($schemas);
    $db->select_db($schema);
    $tables = [];
    foreach ($schemas[$schema] as [$name, $type]) {
        if ($type !== 'BASE TABLE') throw new RuntimeException('Database views are not supported by localization');
        $tables[] = $name;
    }
    $prefixes = [];
    foreach ($tables as $table) {
        if (!str_ends_with($table, 'options')) continue;
        $prefix = substr($table, 0, -7);
        if (in_array($prefix . 'posts', $tables, true) && in_array($prefix . 'users', $tables, true)) $prefixes[] = $prefix;
    }
    $prefix = $request['prefix'] ?: (count($prefixes) === 1 ? $prefixes[0] : '');
    if (!$prefix || !in_array($prefix, $prefixes, true) || !preg_match('/^[a-zA-Z0-9_]+$/D', $prefix)) {
        throw new RuntimeException('Cannot select a unique WordPress table prefix; supply --table-prefix');
    }
    $options = static function ($table) use ($db, $quote) {
        $out = [];
        foreach ($db->query('SELECT option_name,option_value FROM ' . $quote($table) . " WHERE option_name IN ('home','siteurl')")->fetch_all() as [$key, $value]) $out[$key] = $value;
        if (empty($out['home']) || empty($out['siteurl'])) throw new RuntimeException("Missing home/siteurl in $table");
        return $out;
    };
    $multi = in_array($prefix . 'blogs', $tables, true) && in_array($prefix . 'site', $tables, true);
    $result = ['schema'=>$schema, 'prefix'=>$prefix, 'multisite'=>$multi, 'mode'=>'', 'network_id'=>0, 'main_id'=>1, 'sites'=>[], 'tables'=>[], 'no_primary_key'=>[]];
    if ($multi) {
        $networks = $db->query('SELECT id,domain,path FROM ' . $quote($prefix.'site'))->fetch_all(MYSQLI_ASSOC);
        if (count($networks) !== 1) throw new RuntimeException('Only one WordPress network per database is supported');
        $network = $networks[0];
        $result['network_id'] = (int)$network['id'];
        $result['domain'] = $network['domain'];
        $result['base_path'] = $network['path'];
        $modes = [];
        $main = null;
        foreach ($db->query('SELECT blog_id,site_id,domain,path FROM ' . $quote($prefix.'blogs') . ' ORDER BY blog_id')->fetch_all(MYSQLI_ASSOC) as $site) {
            if ((int)$site['site_id'] !== $result['network_id']) throw new RuntimeException('Orphaned or additional network in blogs table');
            $id = (int)$site['blog_id'];
            $table = $prefix . ($id === 1 ? '' : $id . '_') . 'options';
            $values = $options($table);
            $result['sites'][] = ['id'=>$id, 'domain'=>$site['domain'], 'path'=>$site['path'], 'home'=>$values['home'], 'siteurl'=>$values['siteurl']];
            if ($site['domain'] === $network['domain'] && $site['path'] === $network['path']) $main = $id;
            elseif ($site['domain'] === $network['domain']) $modes['subdirectory'] = true;
            elseif (str_ends_with($site['domain'], '.' . $network['domain'])) $modes['subdomain'] = true;
        }
        if ($main === null) throw new RuntimeException('Cannot identify the main site from the network domain/path');
        $result['main_id'] = $main;
        $mode = $request['mode'] ?: (count($modes) === 1 ? array_key_first($modes) : '');
        if (!in_array($mode, ['subdomain','subdirectory'], true)) throw new RuntimeException('Cannot determine network type; supply --multisite-mode');
        if (isset($modes[$mode === 'subdomain' ? 'subdirectory' : 'subdomain'])) throw new RuntimeException('Network type conflicts with registered site domains/paths');
        $result['mode'] = $mode;
    } else {
        $values = $options($prefix.'options');
        $url = parse_url($values['home']);
        $result['domain'] = $url['host'] ?? '';
        $result['base_path'] = rtrim($url['path'] ?? '/', '/') . '/';
        $result['sites'][] = ['id'=>1, 'domain'=>$result['domain'], 'path'=>$result['base_path'], 'home'=>$values['home'], 'siteurl'=>$values['siteurl']];
    }
    foreach ($tables as $table) {
        $result['tables'][] = $table;
        if (!str_starts_with($table, $prefix)) continue;
        if (!$db->query('SHOW KEYS FROM ' . $quote($table) . " WHERE Key_name='PRIMARY'")->num_rows) $result['no_primary_key'][] = $table;
    }
    if ($db->query('SHOW TRIGGERS')->num_rows || $db->query("SELECT ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = '" . $db->real_escape_string($schema) . "'")->num_rows) {
        throw new RuntimeException('Stored routines and triggers require a manual migration');
    }
    // wp is confined to this temporary server; no credentials leave the container.
    $db->query('GRANT ALL ON ' . $quote($schema) . ".* TO 'wp'@'%'");
    echo json_encode($result, JSON_THROW_ON_ERROR);
} catch (Throwable $e) {
    fwrite(STDERR, "Inspect import: " . $e->getMessage() . "\n");
    exit(1);
}
