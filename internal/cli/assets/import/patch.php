<?php
mysqli_report(MYSQLI_REPORT_ERROR | MYSQLI_REPORT_STRICT);
try {
    $r = json_decode(stream_get_contents(STDIN), true, 512, JSON_THROW_ON_ERROR);
    $db = new mysqli('db', 'root', 'root', $r['schema']);
    $db->set_charset('utf8mb4');
    $quote = static fn($s) => '`' . str_replace('`', '``', $s) . '`';
    $update = static function ($sql, $types, ...$args) use ($db) {
        $stmt = $db->prepare($sql);
        $stmt->bind_param($types, ...$args);
        $stmt->execute();
    };
    $prefix = $r['prefix'];
    if ($r['multisite']) {
        $update('UPDATE '.$quote($prefix.'site').' SET domain=?,path=? WHERE id=?', 'ssi', $r['domain'], $r['base_path'], $r['network_id']);
    }
    foreach ($r['sites'] as $site) {
        $id = $site['id'];
        if ($r['multisite']) {
            $update('UPDATE '.$quote($prefix.'blogs').' SET domain=?,path=? WHERE blog_id=?', 'ssi', $site['domain'], $site['path'], $id);
        }
        $table = $quote($prefix . ($r['multisite'] && $id !== 1 ? $id.'_' : '') . 'options');
        $update("UPDATE $table SET option_value=? WHERE option_name='home'", 's', $site['home']);
        $update("UPDATE $table SET option_value=? WHERE option_name='siteurl'", 's', $site['siteurl']);
    }
} catch (Throwable $e) {
    fwrite(STDERR, "Patch import: " . $e->getMessage() . "\n");
    exit(1);
}
