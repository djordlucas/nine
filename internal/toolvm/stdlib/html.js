// nine:html — extracting text and elements from real-world HTML.
//
// A tokenizer, not a tree builder. That is a deliberate scope choice, and it is
// what makes an in-house module defensible here: the two things Nine actually
// does with HTML — flatten a page to readable text, and pull out elements
// carrying a known class — need to know where tags start and end, not how a
// browser would repair mis-nesting. Implicit <tbody>, unclosed <p>, and the rest
// of the HTML5 tree-construction algorithm change the shape of a DOM; they do
// not change which characters are text.
//
// What a tokenizer *must* get right, and what the tests here cover:
//
//   - raw-text elements. Inside <script> and <style>, `<` does not open a tag.
//     Treating it as one is how a naive stripper swallows a page.
//   - comments and doctype, which are not tags and can contain anything.
//   - attribute values that contain ">" — quoted, so the tag has not ended.
//   - unquoted and single-quoted attribute values.
//   - entities, decoded last so "&lt;b&gt;" is text and not markup.
//
// Deliberately absent: selectors beyond a class, anything resembling a DOM, and
// any notion of layout. A tool needing those wants a real parser, which the deps
// pipeline can bundle.

const RAW_TEXT = new Set(["script", "style", "textarea", "title"]);

// Elements after which flattened text gets a line break, so a page reads as
// paragraphs rather than one run-on line.
const BLOCK = new Set([
  "address", "article", "aside", "blockquote", "br", "dd", "div", "dl", "dt",
  "fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4",
  "h5", "h6", "header", "hr", "li", "main", "nav", "ol", "p", "pre", "section",
  "table", "tbody", "td", "th", "thead", "tr", "ul",
]);

// Elements whose content is never readable text.
const DROP = new Set(["script", "style", "noscript", "svg", "head"]);

const ENTITIES = {
  amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: " ",
  "#39": "'", "#x27": "'", "#34": '"', "#x22": '"',
};

/** Decode the entities that actually appear in scraped text. */
export function decodeEntities(s) {
  return String(s).replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z]+);/g, (m, name) => {
    const key = name.toLowerCase();
    if (Object.hasOwn(ENTITIES, key)) return ENTITIES[key];
    if (name[0] === "#") {
      const code = name[1] === "x" || name[1] === "X"
        ? parseInt(name.slice(2), 16)
        : parseInt(name.slice(1), 10);
      if (Number.isFinite(code) && code > 0 && code <= 0x10ffff) {
        try { return String.fromCodePoint(code); } catch { return m; }
      }
    }
    return m; // unknown entity: leave it rather than guess
  });
}

/** Parse an attribute list into a plain object. Values are entity-decoded. */
function parseAttrs(src) {
  const attrs = {};
  const re = /([^\s=/>]+)(?:\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'=<>`]+)))?/g;
  let m;
  while ((m = re.exec(src)) !== null) {
    const name = m[1].toLowerCase();
    const value = m[3] ?? m[4] ?? m[5] ?? "";
    attrs[name] = decodeEntities(value);
  }
  return attrs;
}

/**
 * tokenize(html) -> [{type:"text",value} | {type:"open",name,attrs,selfClosing}
 *                    | {type:"close",name}]
 *
 * Comments, doctypes and processing instructions are skipped entirely.
 */
export function tokenize(html) {
  const s = String(html);
  const out = [];
  let i = 0;
  let raw = null; // when inside a raw-text element, its name

  while (i < s.length) {
    if (raw) {
      // Only the matching close tag ends a raw-text element; every other "<" is
      // literal. This is the case a regex-based stripper gets wrong.
      const close = s.toLowerCase().indexOf(`</${raw}`, i);
      if (close === -1) {
        out.push({ type: "text", value: s.slice(i) });
        break;
      }
      out.push({ type: "text", value: s.slice(i, close) });
      const gt = s.indexOf(">", close);
      out.push({ type: "close", name: raw });
      i = gt === -1 ? s.length : gt + 1;
      raw = null;
      continue;
    }

    const lt = s.indexOf("<", i);
    if (lt === -1) {
      out.push({ type: "text", value: s.slice(i) });
      break;
    }
    if (lt > i) out.push({ type: "text", value: s.slice(i, lt) });

    // Comment, doctype, CDATA — not tags, and their contents are not markup.
    if (s.startsWith("<!--", lt)) {
      const end = s.indexOf("-->", lt + 4);
      i = end === -1 ? s.length : end + 3;
      continue;
    }
    if (s.startsWith("<!", lt) || s.startsWith("<?", lt)) {
      const end = s.indexOf(">", lt);
      i = end === -1 ? s.length : end + 1;
      continue;
    }

    // Find the tag's end, skipping ">" inside quoted attribute values.
    let j = lt + 1;
    let quote = null;
    while (j < s.length) {
      const c = s[j];
      if (quote) {
        if (c === quote) quote = null;
      } else if (c === '"' || c === "'") {
        quote = c;
      } else if (c === ">") {
        break;
      }
      j++;
    }
    if (j >= s.length) {
      out.push({ type: "text", value: s.slice(lt) });
      break;
    }

    const inner = s.slice(lt + 1, j);
    i = j + 1;

    if (inner.startsWith("/")) {
      out.push({ type: "close", name: inner.slice(1).trim().toLowerCase() });
      continue;
    }
    const sp = inner.search(/[\s/]/);
    const name = (sp === -1 ? inner : inner.slice(0, sp)).toLowerCase();
    if (!name) continue;
    const attrSrc = sp === -1 ? "" : inner.slice(sp);
    out.push({
      type: "open",
      name,
      attrs: parseAttrs(attrSrc),
      selfClosing: inner.trimEnd().endsWith("/"),
    });
    if (RAW_TEXT.has(name) && !inner.trimEnd().endsWith("/")) raw = name;
  }
  return out;
}

/** textOf(html) -> the page as readable text, one blank line between blocks. */
export function textOf(html) {
  const parts = [];
  let dropDepth = 0;
  let dropName = null;

  for (const t of tokenize(html)) {
    if (t.type === "open") {
      if (dropDepth > 0) {
        if (t.name === dropName && !t.selfClosing) dropDepth++;
        continue;
      }
      if (DROP.has(t.name)) {
        if (!t.selfClosing) { dropDepth = 1; dropName = t.name; }
        continue;
      }
      if (t.name === "br") parts.push("\n");
      continue;
    }
    if (t.type === "close") {
      if (dropDepth > 0) {
        if (t.name === dropName && --dropDepth === 0) dropName = null;
        continue;
      }
      if (BLOCK.has(t.name)) parts.push("\n");
      continue;
    }
    if (dropDepth > 0) continue;
    const text = decodeEntities(t.value).replace(/\s+/g, " ");
    if (text.trim() !== "") parts.push(text);
  }

  return parts.join("")
    .replace(/[ \t]+/g, " ")
    .replace(/ *\n */g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

/**
 * findByClass(html, className) -> [{text, attrs}] in document order.
 *
 * Matches a whitespace-separated class token, so "result__a" matches
 * class="result__a js-result" and does not match class="result__anchor".
 * Nested elements of the same name are handled with a depth counter, which is
 * the only structural bookkeeping text extraction needs.
 */
export function findByClass(html, className) {
  const want = String(className);
  const found = [];
  const stack = []; // open matches: {name, depth, attrs, parts}

  for (const t of tokenize(html)) {
    if (t.type === "open") {
      for (const f of stack) if (f.name === t.name) f.depth++;
      const classes = String(t.attrs.class ?? "").split(/\s+/);
      if (classes.includes(want) && !t.selfClosing) {
        stack.push({ name: t.name, depth: 1, attrs: t.attrs, parts: [] });
      }
      continue;
    }
    if (t.type === "close") {
      for (let k = stack.length - 1; k >= 0; k--) {
        const f = stack[k];
        if (f.name !== t.name) continue;
        if (--f.depth === 0) {
          found.push({
            text: decodeEntities(f.parts.join("")).replace(/\s+/g, " ").trim(),
            attrs: f.attrs,
          });
          stack.splice(k, 1);
        }
        break;
      }
      continue;
    }
    for (const f of stack) f.parts.push(t.value);
  }
  return found;
}
