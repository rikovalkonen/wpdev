<?php
// wp_debug_mode() resets PHP's startup error_reporting mask while loading
// WordPress. Reapply the import command's mask before running WP-CLI 2.12,
// whose bundled dependencies emit repeated deprecations on PHP 8.5.
// Warnings, notices, exceptions and fatal errors still reach the caller.
WP_CLI::add_hook('after_wp_load', static function () {
    error_reporting(error_reporting() & ~E_DEPRECATED);
});
