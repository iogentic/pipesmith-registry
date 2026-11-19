// Loading a file for the viewer (docs/plans/browse-tools.md, call B5). The detail answer of
// GET /api/asset gives the download address and the size. A file of at most 2 MiB is read whole; a
// larger one is read with one range of its first 2 MiB and loses its last partial line, or, when
// those 2 MiB hold no line end, its last partial character. A file whose
// kind the table does not know is probed with its first 8 KiB first, unless the detail names the
// size 0, because a range on an empty file answers 416. Bytes without a byte order mark are UTF-8,
// unless an XML declaration at their start names another encoding. The viewer draws at most
// 1,000,000 lines.

import { api } from "../api.js";
import { kindOf, isText, utf16Of, PROBE_BYTES } from "./kinds.js";

export const CAP_BYTES = 2 * 1024 * 1024;
export const CAP_LINES = 1_000_000;

// The end of the last whole UTF-8 character of the bytes.
function charEnd(bytes) {
  const n = bytes.length;
  let back = 0;
  while (back < 3 && back < n && (bytes[n - 1 - back] & 0xc0) === 0x80) back += 1;
  const lead = n - 1 - back;
  if (lead < 0) return n;
  const b = bytes[lead];
  const need = b >= 0xf0 ? 4 : b >= 0xe0 ? 3 : b >= 0xc0 ? 2 : 1;
  return need === back + 1 ? n : lead;
}

// The encoding an XML declaration at the start of the bytes names, as the label the decoder knows,
// or null. A byte order mark wins and is looked at before; a label the decoder does not know, and
// UTF-16 without its mark, are left aside.
const DECLARATION = /^<\?xml\s[^>]*?encoding\s*=\s*(?:"([A-Za-z][A-Za-z0-9._:-]*)"|'([A-Za-z][A-Za-z0-9._:-]*)')/;
function declaredEncoding(bytes) {
  if (bytes.length >= 3 && bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf) return null;
  const head = String.fromCharCode(...bytes.subarray(0, Math.min(bytes.length, 512)));
  const found = DECLARATION.exec(head);
  if (!found) return null;
  let decoder;
  try {
    decoder = new TextDecoder(found[1] || found[2]);
  } catch {
    return null;
  }
  return ["utf-8", "utf-16le", "utf-16be", "replacement"].includes(decoder.encoding) ? null : decoder.encoding;
}

// The text of the bytes. A cut file ends at its last whole line, so a character that the cut split
// never shows as a replacement character. A cut file whose loaded part holds no line end ends at its
// last whole character instead, with partialLine set.
function decode(bytes, cut) {
  const wide = utf16Of(bytes);
  let text;
  let partialLine = false;
  if (wide) {
    let even = cut ? bytes.subarray(0, bytes.length - (bytes.length % 2)) : bytes;
    if (cut && even.length >= 2) {
      const n = even.length;
      const unit = wide === "utf-16le" ? even[n - 2] | (even[n - 1] << 8) : (even[n - 2] << 8) | even[n - 1];
      if (unit >= 0xd800 && unit <= 0xdbff) even = even.subarray(0, n - 2);
    }
    text = new TextDecoder(wide).decode(even);
  } else {
    const declared = declaredEncoding(bytes);
    let end = bytes.length;
    if (cut) {
      const newline = bytes.lastIndexOf(0x0a);
      partialLine = newline < 0;
      end = newline >= 0 ? newline + 1 : declared ? bytes.length : charEnd(bytes);
    }
    text = new TextDecoder(declared || "utf-8").decode(bytes.subarray(0, end));
    if (declared) return finish(text, declared, partialLine, false);
  }
  return finish(text, wide || "utf-8", partialLine, Boolean(wide && cut));
}

function finish(text, encoding, partialLine, cutWide) {
  if (text.charCodeAt(0) === 0xfeff) text = text.slice(1);
  if (cutWide) {
    const newline = text.lastIndexOf("\n");
    partialLine = newline < 0;
    if (newline >= 0) text = text.slice(0, newline + 1);
  }
  return { text, encoding, partialLine };
}

// The first CAP_LINES lines of a text, and whether it had more.
function capLines(text) {
  let at = -1;
  for (let i = 0; i < CAP_LINES; i += 1) {
    at = text.indexOf("\n", at + 1);
    if (at < 0) return { text, linesCut: false };
  }
  return at + 1 < text.length ? { text: text.slice(0, at + 1), linesCut: true } : { text, linesCut: false };
}

function refused(error) {
  return { state: "refused", status: error.status, message: error.message, error };
}

// The whole file, or its first CAP_BYTES bytes when the detail names a larger size.
async function read(url, size) {
  const large = typeof size === "number" && size > CAP_BYTES;
  return api.file(url, large ? CAP_BYTES - 1 : null);
}

// Loads the file of a detail answer. The result has state "text" (with text, encoding, size, cut and
// linesCut), "binary", "empty", "refused" (with status and message) or "nourl".
export async function loadFile(info) {
  const url = info && info.download_url;
  if (!url) return { state: "nourl" };
  const kind = kindOf(info.name || info.path, info.content_type);
  let answer;
  try {
    if (kind === "text" && info.size !== 0) {
      answer = await api.file(url, PROBE_BYTES - 1);
      if (answer.status === 416 || !answer.bytes.length) return { state: "empty" };
      if (!isText(answer.bytes)) return { state: "binary", size: answer.total ?? info.size ?? null };
      const whole = answer.status === 200 || (answer.total !== null && answer.total <= answer.bytes.length);
      if (!whole) answer = await read(url, answer.total ?? info.size);
    } else {
      answer = await read(url, info.size);
    }
  } catch (error) {
    if (error && typeof error.status === "number") return refused(error);
    throw error;
  }
  if (answer.status === 416 || !answer.bytes.length) return { state: "empty" };
  let bytes = answer.bytes;
  const size = answer.total ?? info.size ?? bytes.length;
  let cut = size > bytes.length;
  if (bytes.length > CAP_BYTES) {
    bytes = bytes.subarray(0, CAP_BYTES);
    cut = true;
  }
  if (!isText(bytes)) return { state: "binary", size };
  const decoded = decode(bytes, cut);
  const capped = capLines(decoded.text);
  return { state: "text", text: capped.text, encoding: decoded.encoding, size, loaded: bytes.length, cut, partialLine: decoded.partialLine, linesCut: capped.linesCut };
}
