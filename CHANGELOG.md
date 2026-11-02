# Changelog

## 1.0.21 - 2026-11-02

### Features

- update jmail for new mail setup template (`e94a0d3`)

## 1.0.20 - 2026-11-02

### Features

- update duplicity on messages (`9e7f04d`)

## 1.0.19 - 2026-11-02

### Features

- add response signing (`46b71bf`)

### Maintenance

- update install script to output properly (`efc4712`)

## 1.0.18 - 2026-11-02

### Features

- add install script (`13774d5`)
- add script for check-dns for mail (`ce90426`)
- require TLS and restrict senders on submission (`4eec2ac`)
- update template for rate limit (`072d22f`)
- Supabase Auth email hook (`a014d48`)
- add support for multiple users of supabase domain (`9ec411e`)

### Fixes

- script problems (`e4c6ea4`)
- update check-dns script (`ffbc5bd`)
- update check-dns script on dkim checker (`ffe49d8`)
- update script check-dns to fix some issues (`f68433b`)

### Maintenance

- add new script for sending email (`204cf4c`)
- update reconfigure on installer (`a79ddbf`)

## 1.0.17 - 2026-11-01

### CI

- fix mdbook 0.5 --dest-dir (`77201c9`)

## 1.0.16 - 2026-11-01

### CI

- fix cosign-installer ref (`a827e5d`)

## 1.0.15 - 2026-11-01

### Features

- update the deps to latest (`01c2a70`)
- update release script, docs and init scripts (`50b5124`)
- add mk tasks and quadlet generator (`d56349c`)
- add jmail http (`a8a0876`)
- update jmail-http (`47dda0d`)
- add multiple domains support (`bea579a`)

### Documentation

- update jmail readme (`731a710`)

### Maintenance

- add pre-push hook and githooks tasks (`9e27ab6`)

### Other Changes

- Add scripts for testing (`765dfae`)
-  ci: share go checks via a composite action (`c698749`)
