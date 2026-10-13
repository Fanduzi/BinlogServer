// Number field with el-input-number semantics: free typing, clamped to
// [min, max] when it loses focus. nullable fields may be left empty.
import type { ComponentProps } from 'react'
import { Input } from '@/components/ui/input'
import { clampNumber, parseNumberInput } from '@/lib/number'

type Props = Omit<ComponentProps<typeof Input>, 'value' | 'onChange' | 'type' | 'min' | 'max'> & {
  value: number | null | undefined
  onValueChange: (value: number | null) => void
  min: number
  max?: number
  nullable?: boolean
}

export function NumberInput({ value, onValueChange, min, max, nullable, onBlur, ...rest }: Props) {
  return (
    <Input
      {...rest}
      type='number'
      min={min}
      max={max}
      value={value ?? ''}
      onChange={(e) => {
        const n = parseNumberInput(e.target.value)
        onValueChange(n == null && !nullable ? 0 : n)
      }}
      onBlur={(e) => {
        if (!(nullable && value == null)) onValueChange(clampNumber(value, min, max))
        onBlur?.(e)
      }}
    />
  )
}
