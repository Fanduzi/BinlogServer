// Number field helpers shared by every form (el-input-number semantics).
export function parseNumberInput(text: string): number | null {
  const trimmed = text.trim()
  if (trimmed === '') return null
  const n = Number(trimmed)
  return Number.isFinite(n) ? n : null
}

/** Clamp to [min, max]; a missing or non-finite value becomes min. */
export function clampNumber(
  value: number | null | undefined,
  min: number,
  max = Number.MAX_SAFE_INTEGER,
) {
  if (value == null || !Number.isFinite(value)) return min
  return Math.min(max, Math.max(min, value))
}
