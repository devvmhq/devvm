// Draws every mermaid code block in docs/content as an SVG, with pele (docs/package.json), before Hugo
// builds the site, so its pages load no diagram JavaScript. Each block's files go in
// docs/.build/diagrams/, which git ignores, named for its key: <key>.svg, and <key>.json with its
// width and height. The render hook (layouts/_markup/render-codeblock-mermaid.html) computes the same
// key and inlines the SVG.
//
// A key is a hash of the block's text and of the salt, which is itself a hash of pele's version and
// this script's text, its render settings included; the salt is written to the folder's salt file for
// the hook. A drawing whose files are already there is kept, only the missing ones are drawn, and
// files no block's key names are deleted, so a run with nothing changed draws nothing, and a new pele
// or a change here draws them all again.
//
// The theme is CSS: custom.css sets pele's --pele-* variables. The layout is set here: Departure Mono
// at 14px, measured from the font's metrics, and the Mermaid settings below.
//
// Run it with mise run docs:diagrams, which installs pele first (npm ci).
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { render } from "pele";

const script = fileURLToPath(import.meta.url);
const docs = dirname(script);
const out = join(docs, ".build", "diagrams");
const SALT_FILE = "salt";

// Departure Mono: 550 units to the em and every glyph 350 wide, so a character's width is its size
// times 350/550, and the measurer counts characters.
const FONT = '"Departure Mono", monospace';
const ADVANCE = 350 / 550;
const SIZE = 14;
// pele draws edge labels, sequence messages, notes and boxes smaller than the rest, at
// round(size * 0.875), 12px, with no option to change it. The measurer gives that text the width it
// has at 14px, so pele lays it out for 14px, and grow() then draws it at 14px.
const SMALL = Math.round(SIZE * 0.875);
const measured = [];
const measurer = {
  width(text, size) {
    measured.push([text, size]);
    return Array.from(text).length * (size === SMALL ? SIZE : size) * ADVANCE;
  },
};

const options = (key) => ({
  measurer,
  fontFamily: FONT,
  fontFamilyMono: FONT,
  fontSize: SIZE,
  idPrefix: `d${key}-`,
  // At their own size, never scaled to fit or turned: custom.css scrolls a wide one sideways.
  responsive: false,
  autoDirection: false,
  links: false,
  images: false,
  // Defaults a diagram's front matter can override. step draws edges with right angles.
  config: {
    flowchart: { curve: "step", wrappingWidth: 340, nodeSpacing: 45, rankSpacing: 45 },
    sequence: { mirrorActors: false, actorMargin: 45, diagramMarginX: 1 },
  },
});

// grow: the small text's font-size, which pele writes on each group of it, at 14px. Every such group
// must carry font-size="SMALL", and a diagram with small text but none of those groups means pele has
// changed how it writes it.
const SMALL_GROUP = /<g class="pele-(edge-labels|notes|boxes|frames)"[^>]*>/g;
const SMALL_ITEM = /class="pele-(edge-label|note|box|frame)/;
const SMALL_SIZE = new RegExp(`\\sfont-size="${SMALL}"`);
function grow(svg, where) {
  const groups = svg.match(SMALL_GROUP) ?? [];
  if (SMALL_ITEM.test(svg) && !groups.length) {
    throw new Error(`${where}: pele drew small text outside its groups: check grow() against this pele`);
  }
  for (const g of groups) {
    if (!SMALL_SIZE.test(g)) {
      throw new Error(`${where}: pele drew ${g} without font-size="${SMALL}": check grow() against this pele`);
    }
  }
  return svg.replace(SMALL_GROUP, (g) => g.replace(SMALL_SIZE, ` font-size="${SIZE}"`));
}

// A newline after each text element, outside it, where SVG draws nothing, so the site's search index,
// which reads a page's text without its tags, sees each label's last word apart from the next label's
// first. The lines of one label are tspans in one text element, where a newline would be a space
// that moves a centered line, so they stay joined.
const searchable = (svg) => svg.replaceAll("</text>", "</text>\n");

// Checks the things about pele that grow() relies on, which its docs don't promise: an edge label, a
// message and a note are measured at SMALL, and drawn in a group with font-size="SMALL".
function selfCheck() {
  const cases = [
    ["flowchart", "flowchart TD\n  a --> |edge label| b", "edge label", "edge-labels"],
    ["sequence", "sequenceDiagram\n  A->>B: a message", "a message", "edge-labels"],
    ["note", "sequenceDiagram\n  A->>B: hi\n  Note right of B: a note", "a note", "notes"],
  ];
  for (const [name, source, label, group] of cases) {
    measured.length = 0;
    const { svg } = render(source, options(`check-${name}`));
    const sizes = measured.filter(([t]) => t === label).map(([, s]) => s);
    if (!sizes.length || sizes.some((s) => s !== SMALL)) {
      throw new Error(`self-check: pele measured the ${name}'s "${label}" at ${sizes.join(", ") || "no size"}, not ${SMALL}px`);
    }
    const tag = (svg.match(new RegExp(`<g class="pele-${group}"[^>]*>`)) ?? [""])[0];
    if (!SMALL_SIZE.test(tag)) {
      throw new Error(`self-check: pele's ${name} has no <g class="pele-${group}" font-size="${SMALL}">`);
    }
  }
}

// A diagram's accessible name and description: pele makes its accTitle and accDescr the SVG's
// <title> and <desc>, which screen readers read, so every diagram needs both.
function accessible(svg, where) {
  if (!/aria-labelledby="[^"]*-title"/.test(svg) || !/aria-describedby="[^"]*-desc"/.test(svg)) {
    throw new Error(`${where}: give the diagram an accTitle and an accDescr, which screen readers read`);
  }
}

function* markdownFiles(dir) {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) yield* markdownFiles(p);
    else if (e.name.endsWith(".md")) yield p;
  }
}

// The blocks, fenced at the start of a line; an indented one would reach the hook with other text.
function diagrams() {
  const found = [];
  for (const file of markdownFiles(join(docs, "content"))) {
    const text = readFileSync(file, "utf8");
    const where = (index) => `${relative(docs, file)}:${text.slice(0, index).split("\n").length}`;
    for (const m of text.matchAll(/^[ \t]+```mermaid/gm)) {
      throw new Error(`${where(m.index)}: an indented mermaid block; start its fence at the line's start`);
    }
    for (const m of text.matchAll(/^```mermaid[ \t]*\n([\s\S]*?)\n```[ \t]*$/gm)) {
      found.push({ where: where(m.index), inner: m[1] });
    }
  }
  return found;
}

const sha256 = (text) => createHash("sha256").update(text).digest("hex").slice(0, 16);

// The salt: the first 16 hex digits of the SHA-256 of pele's installed version and this script.
function salt() {
  const pele = JSON.parse(readFileSync(join(docs, "node_modules", "pele", "package.json"), "utf8"));
  return sha256(`pele ${pele.version}\n${readFileSync(script, "utf8")}`);
}

// The key the render hook computes: the first 16 hex digits of the SHA-256 of the salt, a newline and
// the block's text, which is Hugo's .Inner, the lines between the fences without the last newline.
const key = (s, inner) => sha256(`${s}\n${inner}`);

// Written beside its final name and renamed, so a run stopped halfway leaves no partial drawing that a
// later run would keep.
function put(name, data) {
  writeFileSync(join(out, `${name}.tmp`), data);
  renameSync(join(out, `${name}.tmp`), join(out, name));
}

try {
  const s = salt();
  const all = diagrams().map((d) => ({ ...d, key: key(s, d.inner) }));
  mkdirSync(out, { recursive: true });
  const missing = all.filter((d) => !existsSync(join(out, `${d.key}.svg`)) || !existsSync(join(out, `${d.key}.json`)));
  // Only when there's something to draw, as a drawing already there passed it when it was drawn.
  if (missing.length) selfCheck();
  for (const d of missing) {
    const { svg, width, height } = render(d.inner, options(d.key));
    accessible(svg, d.where);
    // The size first: the hook needs both, and the SVG's presence is what says it's drawn.
    put(`${d.key}.json`, JSON.stringify({ width: Math.ceil(width), height: Math.ceil(height) }) + "\n");
    put(`${d.key}.svg`, searchable(grow(svg, d.where)));
  }
  put(SALT_FILE, s);
  const keep = new Set([SALT_FILE, ...all.flatMap((d) => [`${d.key}.svg`, `${d.key}.json`])]);
  const unused = readdirSync(out).filter((f) => !keep.has(f));
  for (const f of unused) rmSync(join(out, f), { force: true });
  // The folder is mise's output for docs:diagrams, which it compares with the sources' times.
  const now = new Date();
  utimesSync(out, now, now);
  console.log(
    `render-diagrams: ${all.length} diagrams in ${relative(process.cwd(), out) || "."}, ${missing.length} drawn, ${unused.length} files deleted`,
  );
} catch (e) {
  console.error(`render-diagrams: ${e.message}`);
  process.exit(1);
}
