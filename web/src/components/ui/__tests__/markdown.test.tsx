/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, expect, test } from 'vitest'

import { Markdown } from '../markdown'

afterEach(cleanup)

test.each(['Inline $$\\frac{x}{2}$$ content', '$$\n\\frac{x}{2}\n$$'])(
  'retains accessible MathML for valid math: %s',
  (source) => {
    const { container } = render(<Markdown>{source}</Markdown>)

    expect(container.querySelector('math mfrac')).not.toBeNull()
    expect(container.querySelector('annotation')?.textContent).toBe(
      '\\frac{x}{2}'
    )
  }
)

test('sanitizes embedded HTML without losing a safe document link', () => {
  const { container } = render(
    <Markdown baseUrl='https://example.com/releases/'>
      {
        '<script>alert(1)</script><img src="x" onerror="alert(1)">\n\n[safe](notes.md)'
      }
    </Markdown>
  )

  expect(container.querySelector('script, [onerror]')).toBeNull()
  expect(screen.getByRole('link', { name: 'safe' })).toHaveAttribute(
    'href',
    'https://example.com/releases/notes.md'
  )
})

test('does not turn untrusted math link commands into executable links', () => {
  const { container } = render(
    <Markdown>
      {'Inline $$\\href{javascript:alert(1)}{unsafe}$$ content'}
    </Markdown>
  )

  expect(container.querySelector('a[href^="javascript:"]')).toBeNull()
  expect(container.querySelector('script, [onclick], [onerror]')).toBeNull()
})
