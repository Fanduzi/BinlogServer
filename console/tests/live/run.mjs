// Runs every live check in order against a freshly seeded ui302 env
// (see README.md). rd.mjs deletes a task, so it runs last.
import { spawnSync } from 'node:child_process'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const dir = dirname(fileURLToPath(import.meta.url))
const only = process.argv.slice(2)
const all = ['rc', 'ov', 'src', 'wk', 'fixes', 'auth-inputs', 'rd']
let failed = 0
for (const name of only.length ? only : all) {
  console.log(`\n=== ${name}`)
  const r = spawnSync(process.execPath, [join(dir, `${name}.mjs`)], { stdio: 'inherit' })
  if (r.status !== 0) {
    failed++
    console.log(`=== ${name}: FAILED (exit ${r.status})`)
  }
}
console.log(failed ? `\n${failed} script(s) failed` : '\nall live checks passed')
process.exit(failed ? 1 : 0)
