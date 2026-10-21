<?php
// This program parses source; it never includes/evaluates the supplied config.
function wpdev_transform_config(string $source, array $values): array {
    // Restore our original statements before applying the current settings.
    $source = preg_replace_callback(
        '~\/\* wpdev:original ([A-Za-z0-9+/=]+) \*\/.*?\/\* wpdev:end \*\/~s',
        static function ($m) {
            $original = base64_decode($m[1], true);
            if ($original === false) throw new RuntimeException('Invalid wpdev config marker');
            return $original;
        }, $source
    );
    $source = preg_replace('~\/\* wpdev:additions \*\/.*?\/\* wpdev:additions:end \*\/\r?\n?~s', '', $source);
    $tokens = [];
    $offset = 0;
    $scope = 0;
    foreach (token_get_all($source, TOKEN_PARSE) as $t) {
        $text = is_array($t) ? $t[1] : $t;
        $id = is_array($t) ? $t[0] : null;
        if (!in_array($id, [T_WHITESPACE, T_COMMENT, T_DOC_COMMENT], true)) {
            $tokens[] = [$id, $text, $offset, $offset + strlen($text), $scope];
        }
        if ($id === null && $text === '{') $scope++;
        if ($id === null && $text === '}') $scope--;
        $offset += strlen($text);
    }
    $definitions = [];
    $hints = [];
    $count = count($tokens);
    for ($i = 0; $i < $count; $i++) {
        $t = $tokens[$i];
        $variable = $t[0] === T_VARIABLE && $t[1] === '$table_prefix' && ($tokens[$i+1][1] ?? '') === '=';
        $define = $t[0] === T_STRING && strtolower($t[1]) === 'define' && ($tokens[$i+1][1] ?? '') === '(';
        if (!$variable && !$define) continue;
        if ($define) {
            $nameToken = $tokens[$i+2] ?? [];
            if (($nameToken[0] ?? null) !== T_CONSTANT_ENCAPSED_STRING || ($tokens[$i+3][1] ?? '') !== ',') continue;
            $name = substr($nameToken[1], 1, -1);
            if (!preg_match('/^[A-Z_]+$/D', $name)) continue;
            $start = $i + 4;
        } else {
            $name = 'table_prefix';
            $start = $i + 2;
        }
        if (array_key_exists($name, $values)) {
            $previous = $tokens[$i-1] ?? [T_OPEN_TAG, '<?php'];
            if ($t[4] !== 0 || ($previous[0] !== T_OPEN_TAG && !in_array($previous[1], [';', '}'], true))) {
                throw new RuntimeException("$name must be a direct top-level statement; move conditional definitions into its value expression");
            }
        }
        $depth = 0;
        $end = null;
        for ($j = $start; $j < $count; $j++) {
            $text = $tokens[$j][1];
            if ($tokens[$j][0] === null) {
                if ($depth === 0 && ($variable ? $text === ';' : ($text === ')' || $text === ','))) {
                    $end = $j;
                    break;
                }
                if (in_array($text, ['(', '[', '{'], true)) $depth++;
                if (in_array($text, [')', ']', '}'], true)) $depth--;
            }
        }
        if ($end === null) throw new RuntimeException("Cannot parse $name");
        $statementEnd = $end;
        if ($define) {
            // Do not rewrite the deprecated third argument or compound expressions.
            if ($tokens[$end][1] !== ')' || ($tokens[$end+1][1] ?? '') !== ';') {
                if (array_key_exists($name, $values)) throw new RuntimeException("Unsupported definition of $name; use a direct define(name, value) statement");
                continue;
            }
            $statementEnd++;
        }
        $exprStart = $tokens[$start][2];
        $exprEnd = $tokens[$end-1][3];
        $expression = substr($source, $exprStart, $exprEnd - $exprStart);
        $definitions[$name][] = [$t[2], $tokens[$statementEnd][3], $exprStart, $exprEnd, $expression];
        if ($name === 'SUBDOMAIN_INSTALL' && in_array(strtolower(trim($expression)), ['true', 'false'], true)) {
            $hints['mode'] = strtolower(trim($expression)) === 'true' ? 'subdomain' : 'subdirectory';
        }
    }
    $edits = [];
    $additions = [];
    $anchor = null;
    foreach ($values as $name => $setting) {
        $found = $definitions[$name] ?? [];
        if (count($found) > 1) throw new RuntimeException("Multiple definitions of $name; consolidate them before running config local");
        $required = str_starts_with($name, 'DB_') || $name === 'table_prefix';
        if (!$found) {
            if ($required) throw new RuntimeException("$name must be defined directly in wp-config.php; definitions in included files or const declarations require manual adjustment");
            if ($setting !== null) $additions[] = "define(" . var_export($name, true) . ", " . var_export($setting, true) . ");";
            continue;
        }
        [$from, $to, $exprStart, $exprEnd, $expression] = $found[0];
        $anchor = $anchor === null ? $from : min($anchor, $from);
        $original = substr($source, $from, $to - $from);
        if ($setting === null) {
            // Site-specific DB options must win over production global URL constants.
            $replacement = "if (getenv('WPDEV_LOCAL') !== '1') { $original }";
        } else {
            $local = var_export($setting, true);
            if (str_starts_with($name, 'DB_')) {
                $env = var_export('WPDEV_' . $name, true);
                $local = "(getenv($env) !== false ? getenv($env) : $local)";
            }
            $replacement = substr($source, $from, $exprStart - $from)
                . "(getenv('WPDEV_LOCAL') === '1' ? $local : ($expression))"
                . substr($source, $exprEnd, $to - $exprEnd);
        }
        $edits[] = [$from, $to, '/* wpdev:original ' . base64_encode($original) . " */\n" . $replacement . "\n/* wpdev:end */"];
    }
    if ($additions) {
        if ($anchor === null) throw new RuntimeException('No safe config insertion point');
        $edits[] = [$anchor, $anchor, "/* wpdev:additions */\nif (getenv('WPDEV_LOCAL') === '1') {\n"
            . implode("\n", $additions) . "\n}\n/* wpdev:additions:end */\n"];
    }
    // At the same offset replace the original statement before inserting additions.
    usort($edits, static fn($a, $b) => ($b[0] <=> $a[0]) ?: ($b[1] <=> $a[1]));
    foreach ($edits as [$from, $to, $replacement]) {
        $source = substr_replace($source, $replacement, $from, $to - $from);
    }
    token_get_all($source, TOKEN_PARSE);
    return ['source' => $source, 'hints' => (object)$hints];
}

try {
    $request = json_decode(stream_get_contents(STDIN), true, 512, JSON_THROW_ON_ERROR);
    echo json_encode(wpdev_transform_config($request['source'], $request['values']), JSON_THROW_ON_ERROR);
} catch (Throwable $e) {
    fwrite(STDERR, "wp-config: " . $e->getMessage() . "\n");
    exit(1);
}
