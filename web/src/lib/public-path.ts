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

/** Shared by build configuration and browser owners; no browser globals. */
export function normalizeBasePath(value: string = ''): string {
  if (value === '' || value === '/') return ''
  if (
    /[^A-Za-z0-9_/-]/.test(value) ||
    !/^\/[A-Za-z0-9_-]+(?:\/[A-Za-z0-9_-]+)*\/?$/.test(value)
  ) {
    throw new Error(
      'APP_BASE_PATH must be an absolute local path of ASCII segments'
    )
  }
  return value.endsWith('/') ? value.slice(0, -1) : value
}

/** Only owned single-slash paths are mounted; suffix encoding is untouched. */
export function joinPublicPath(value: string, prefix: string): string {
  if (!prefix || !value.startsWith('/') || value.startsWith('//')) return value
  const pathname = value.split(/[?#]/, 1)[0]
  if (pathname === prefix || pathname.startsWith(`${prefix}/`)) return value
  return `${prefix}${value}`
}

export function validateSourceUrl(value: string, prefix: string): string {
  if (!value) return ''
  // Immutable archives under /source/, or the explicit first-party archive
  // endpoint requested by the deployment owner. Never accept remote URLs.
  const archive = value.slice(prefix.length)
  if (
    !value.startsWith(`${prefix}/`) ||
    /[^A-Za-z0-9_./-]/.test(value) ||
    !/^\/(?:source\/[A-Za-z0-9_-]+(?:\/[A-Za-z0-9_-]+)*\.tar\.gz|source\.tar\.gz)$/.test(
      archive
    )
  ) {
    throw new Error(
      'VITE_CORRESPONDING_SOURCE_URL must be a local source archive beneath APP_BASE_PATH'
    )
  }
  return value
}
