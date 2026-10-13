// Shared settings for the live checks. BASE is the Console, CLASSIC the Vue
// console used as the reference; both default to the embedded binary.
import { mkdirSync } from 'node:fs'

const server = process.env.SERVER || 'http://127.0.0.1:18302'
export const BASE = process.env.BASE || `${server}/ui`
export const CLASSIC = process.env.CLASSIC || `${server}/ui-classic`
export const OUT = process.env.OUT || '/tmp/console-live'
mkdirSync(`${OUT}/detail`, { recursive: true })
