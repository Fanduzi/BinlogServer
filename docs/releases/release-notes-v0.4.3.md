# Binlog Server v0.4.3

Release date: 2026-08-30

Binlog Server `v0.4.3` closes a metadata/source isolation bypass where equivalent loopback host spellings could be treated as different sources.

## Highlights

- Metadata/source checks now treat `localhost`, IPv4 loopback literals in `127/8`, and IPv6 loopback literals such as `::1` as one same-port endpoint identity. Bracketed or expanded IPv6 and normalized `localhost` spellings are covered; arbitrary hostnames are not resolved through DNS.
- Create, source configuration/update, and start reject a source in the metadata endpoint's loopback class. Rejected creates are not persisted, and a different port remains allowed.
- Source lookup uses the same loopback identity, so aliases return the same task matches. Non-loopback hosts keep their existing exact-text behavior.

## Upgrade Notes

- No schema migration is required for `v0.4.3`.
- Existing stored source host text is not rewritten. Keep the metadata MySQL instance separate from every replication source.

## Chinese Release Notes

Chinese version:

https://github.com/Fanduzi/BinlogServer/blob/main/docs/releases/v0.4.3.zh-CN.md
