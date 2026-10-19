// input: the flag and path lines of one binlog client invocation
// output: the full copy-paste restore command Playwright expects from the Console
// pos: shared expected text for multi-file restore commands
// note: if this file changes, update this header and console/tests/e2e/fixtures/README.md.

export function mysqlRestoreCommand(body: string[]) {
  return [
    'set -o pipefail',
    'TZ=UTC mysqlbinlog \\',
    ...body,
    '# Pipe stdout to mysql in this same shell so pipefail sees a client failure.',
    '# Pass every file to one mysqlbinlog, in order. Do not restore one file at a time.',
    "# Verify: mysql -Nse 'SELECT COUNT(*) FROM db.tbl; SELECT @@gtid_executed'",
  ].join('\n')
}
