import { Fragment, type ReactNode } from 'react'

// A tiny self-contained Markdown renderer covering the subset used by the
// documentation pages: headings (# .. ####), paragraphs, bullet and numbered
// lists (with nesting), fenced code blocks, tables, blockquotes, inline
// code / bold / italic / links. It builds React nodes directly — no
// dangerouslySetInnerHTML.

// --- inline parsing ---------------------------------------------------------

// Inline patterns are tried in order at each position; `code` spans win
// first so their contents are not further formatted.
const INLINE_PATTERNS: { re: RegExp; render: (m: RegExpMatchArray, key: number) => ReactNode }[] = [
  { re: /^`([^`]+)`/, render: (m, key) => <code key={key}>{m[1]}</code> },
  { re: /^\*\*([^*]+)\*\*/, render: (m, key) => <strong key={key}>{m[1]}</strong> },
  { re: /^\*([^*]+)\*/, render: (m, key) => <em key={key}>{m[1]}</em> },
  {
    re: /^\[([^\]]+)\]\(([^)]+)\)/,
    render: (m, key) => (
      <a key={key} href={m[2]} target="_blank" rel="noreferrer">
        {m[1]}
      </a>
    ),
  },
]

function renderInline(text: string): ReactNode[] {
  const out: ReactNode[] = []
  let rest = text
  let key = 0
  while (rest.length > 0) {
    // Next special character decides where the plain-text run ends.
    const next = rest.search(/[`*[]/)
    if (next < 0) {
      out.push(rest)
      break
    }
    if (next > 0) {
      out.push(rest.slice(0, next))
      rest = rest.slice(next)
    }
    let matched = false
    for (const p of INLINE_PATTERNS) {
      const m = rest.match(p.re)
      if (m) {
        out.push(p.render(m, key++))
        rest = rest.slice(m[0].length)
        matched = true
        break
      }
    }
    if (!matched) {
      // A lone special character: emit it literally.
      out.push(rest[0])
      rest = rest.slice(1)
    }
  }
  return out
}

// --- block parsing ----------------------------------------------------------

type Block =
  | { kind: 'heading'; level: number; text: string }
  | { kind: 'paragraph'; text: string }
  | { kind: 'code'; lang: string; lines: string[] }
  | { kind: 'list'; ordered: boolean; items: ListItem[] }
  | { kind: 'table'; header: string[]; rows: string[][] }
  | { kind: 'quote'; text: string }

interface ListItem {
  // Own inline text; children are nested list items (one level of nesting is
  // enough for the docs).
  text: string
  children: ListItem[]
}

function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n/g, '\n').split('\n')
  const blocks: Block[] = []
  let i = 0

  const splitRow = (line: string): string[] =>
    line
      .replace(/^\s*\|/, '')
      .replace(/\|\s*$/, '')
      // Split on unescaped pipes only; \| is a literal | inside a cell.
      .split(/(?<!\\)\|/)
      .map((c) => c.trim().replace(/\\\|/g, '|'))

  const isTableDivider = (line: string) =>
    /^\s*\|?[\s:|-]+\|?\s*$/.test(line) && line.includes('-')

  while (i < lines.length) {
    const line = lines[i]

    // Blank line between blocks.
    if (line.trim() === '') {
      i++
      continue
    }

    // Fenced code block.
    if (line.startsWith('```')) {
      const lang = line.slice(3).trim()
      i++
      const codeLines: string[] = []
      while (i < lines.length && !lines[i].startsWith('```')) {
        codeLines.push(lines[i])
        i++
      }
      i++ // closing fence
      blocks.push({ kind: 'code', lang, lines: codeLines })
      continue
    }

    // Heading.
    const h = line.match(/^(#{1,4})\s+(.*)$/)
    if (h) {
      blocks.push({ kind: 'heading', level: h[1].length, text: h[2] })
      i++
      continue
    }

    // Table: a row followed by a divider row.
    if (line.includes('|') && i + 1 < lines.length && isTableDivider(lines[i + 1])) {
      const header = splitRow(line)
      i += 2
      const rows: string[][] = []
      while (i < lines.length && lines[i].includes('|') && lines[i].trim() !== '') {
        rows.push(splitRow(lines[i]))
        i++
      }
      blocks.push({ kind: 'table', header, rows })
      continue
    }

    // Blockquote.
    if (line.startsWith('>')) {
      const parts: string[] = []
      while (i < lines.length && lines[i].startsWith('>')) {
        parts.push(lines[i].replace(/^>\s?/, ''))
        i++
      }
      blocks.push({ kind: 'quote', text: parts.join(' ') })
      continue
    }

    // Lists: consecutive `- ` / `* ` / `1. ` lines, with two-space indented
    // sub-items.
    const bullet = line.match(/^(\s*)([-*])\s+(.*)$/)
    const ordered = line.match(/^(\s*)(\d+)\.\s+(.*)$/)
    if (bullet || ordered) {
      const isOrdered = !!ordered
      const items: ListItem[] = []
      while (i < lines.length) {
        const cur = lines[i]
        const m = isOrdered ? cur.match(/^(\s*)(\d+)\.\s+(.*)$/) : cur.match(/^(\s*)([-*])\s+(.*)$/)
        if (m) {
          const indent = m[1].length
          if (indent >= 2 && items.length > 0) {
            items[items.length - 1].children.push({ text: m[3], children: [] })
          } else {
            items.push({ text: m[3], children: [] })
          }
          i++
          continue
        }
        // Non-empty continuation lines append to the last item.
        if (cur.trim() !== '' && items.length > 0 && !/^\s*([-*]|\d+\.)\s/.test(cur)) {
          items[items.length - 1].text += ' ' + cur.trim()
          i++
          continue
        }
        break
      }
      blocks.push({ kind: 'list', ordered: isOrdered, items })
      continue
    }

    // Paragraph: consecutive non-special lines.
    const parts: string[] = []
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !lines[i].startsWith('```') &&
      !/^#{1,4}\s/.test(lines[i]) &&
      !/^\s*([-*])\s+/.test(lines[i]) &&
      !/^\s*\d+\.\s/.test(lines[i]) &&
      !lines[i].startsWith('>') &&
      !(lines[i].includes('|') && i + 1 < lines.length && isTableDivider(lines[i + 1]))
    ) {
      parts.push(lines[i].trim())
      i++
    }
    if (parts.length > 0) {
      blocks.push({ kind: 'paragraph', text: parts.join(' ') })
    } else {
      i++ // safety: never loop forever on an unrecognized line
    }
  }
  return blocks
}

function renderListItem(item: ListItem, key: number): ReactNode {
  return (
    <li key={key}>
      {renderInline(item.text)}
      {item.children.length > 0 && (
        <ul>
          {item.children.map((c, idx) => renderListItem(c, idx))}
        </ul>
      )}
    </li>
  )
}

// --- top-level renderer -----------------------------------------------------

export function Markdown({ source }: { source: string }) {
  const blocks = parseBlocks(source)
  return (
    <div className="md-doc">
      {blocks.map((b, idx) => {
        switch (b.kind) {
          case 'heading': {
            if (b.level === 1) return <h2 key={idx}>{renderInline(b.text)}</h2>
            if (b.level === 2) return <h3 key={idx}>{renderInline(b.text)}</h3>
            if (b.level === 3) return <h4 key={idx}>{renderInline(b.text)}</h4>
            return <h5 key={idx} className="md-h5">{renderInline(b.text)}</h5>
          }
          case 'paragraph':
            return <p key={idx}>{renderInline(b.text)}</p>
          case 'code':
            return (
              <pre key={idx} className="output">
                <code>{b.lines.join('\n')}</code>
              </pre>
            )
          case 'quote':
            return (
              <blockquote key={idx} className="md-quote">
                {renderInline(b.text)}
              </blockquote>
            )
          case 'list':
            return b.ordered ? (
              <ol key={idx}>{b.items.map((it, j) => renderListItem(it, j))}</ol>
            ) : (
              <ul key={idx}>{b.items.map((it, j) => renderListItem(it, j))}</ul>
            )
          case 'table':
            return (
              <table key={idx} className="table md-table">
                <thead>
                  <tr>
                    {b.header.map((c, j) => (
                      <th key={j}>{renderInline(c)}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {b.rows.map((row, r) => (
                    <tr key={r}>
                      {row.map((c, j) => (
                        <td key={j}>{renderInline(c)}</td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            )
          default:
            return <Fragment key={idx} />
        }
      })}
    </div>
  )
}
