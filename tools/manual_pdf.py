#!/usr/bin/env python3
"""Builds the manual as a PDF, checks that it is fresh, and renders its pages.

usage:
  uv run --no-project --with playwright --with pypdf python3 tools/manual_pdf.py
  python3 tools/manual_pdf.py --check
  python3 tools/manual_pdf.py --pages DIR

Default run: serves docs/ from 127.0.0.1 on a free port between 14100 and
14999, with the Content-Security-Policy of internal/ui/ui.go on every answer.
The installed Chrome opens index.html at localhost with print media, waits
for the figures and the table of operations, and calls
window.pipesmithManual.preparePrint() of manual.js. Pass 1 renders the PDF
with empty page numbers in the contents and finds the page of every numbered
heading: by the named destination of its id, else by the first page after the
contents whose text holds a line that starts with the number and the title of
the heading. Pass 2 prepares the page with those numbers and renders again.
The run stops with exit 1 on a console error, a page error, a failed request,
an answer of 400 or more, a securitypolicyviolation event, a heading without
a page, a different page count between the passes, or a contents number that
differs from the page of its heading in pass 2. The dates Chrome writes into
the PDF are replaced by a fixed date of the same length, so two runs give the
same bytes. It writes docs/pdf/pipesmith-registry-manual.pdf and beside it
pipesmith-registry-manual.source: in line 1 the SHA-256 over the files the
PDF is built from (see source_hash), in line 2 the SHA-256 of the PDF.

--check uses the standard library only: it recomputes both hashes, compares
them with the two lines of the .source file, checks that the PDF starts with
%PDF- and is larger than 50,000 bytes, and prints one line when something
differs (exit 1). A .source file of one line, written before the PDF digest,
fails: the PDF must be built again.

--pages DIR renders every page to DIR/page-NN.png at 100 dots per inch with
pdftoppm, else with pypdfium2 (uv run --no-project --with pypdfium2).
"""
from __future__ import annotations

import hashlib
import http.server
import io
import random
import re
import shutil
import socket
import subprocess
import sys
import threading
from functools import partial
from pathlib import Path

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "tools"))

from sync_manual import manual_set  # noqa: E402

DOCS = ROOT / "docs"
PDF = DOCS / "pdf" / "pipesmith-registry-manual.pdf"
SOURCE_NAME = "pipesmith-registry-manual.source"
# Pages and scripts of the manual set that the PDF does not show.
NOT_SHOWN = ("api.html", "api.js", "api.css")
REBUILD = "uv run --no-project --with playwright --with pypdf python3 tools/manual_pdf.py"
MIN_BYTES = 50_000
PORTS = range(14100, 15000)
WAIT_MS = 15_000
FIGURES = ("architecture", "take-over")
FIXED_DATE = "20000101000000000000"


def sources(docs: Path = DOCS) -> dict[str, Path]:
    """The files the PDF is built from: the manual set without the API page and without the PDF."""
    return {name: path for name, path in manual_set(docs).items()
            if name not in NOT_SHOWN and not name.startswith("pdf/")}


def source_hash(docs: Path = DOCS) -> str:
    """SHA-256 over the sorted lines "path NUL sha256(file)", each ended by a newline."""
    lines = sorted(f"{name}\0{hashlib.sha256(path.read_bytes()).hexdigest()}\n" for name, path in sources(docs).items())
    return hashlib.sha256("".join(lines).encode("utf-8")).hexdigest()


def source_file(docs: Path = DOCS) -> Path:
    return docs / PDF.relative_to(DOCS).parent / SOURCE_NAME


def pdf_hash(docs: Path = DOCS) -> str:
    return hashlib.sha256((docs / PDF.relative_to(DOCS)).read_bytes()).hexdigest()


def write_source(docs: Path = DOCS) -> None:
    """Line 1: source_hash. Line 2: the SHA-256 of the PDF."""
    source_file(docs).write_text(source_hash(docs) + "\n" + pdf_hash(docs) + "\n", encoding="utf-8")


def check(docs: Path = DOCS) -> int:
    pdf = docs / PDF.relative_to(DOCS)
    side = source_file(docs)
    problem = None
    if not pdf.is_file():
        problem = f"{pdf.relative_to(docs.parent)} is missing"
    elif pdf.stat().st_size <= MIN_BYTES:
        problem = f"{pdf.relative_to(docs.parent)} has {pdf.stat().st_size} bytes, want more than {MIN_BYTES}"
    elif not pdf.read_bytes().startswith(b"%PDF-"):
        problem = f"{pdf.relative_to(docs.parent)} does not start with %PDF-"
    elif not side.is_file():
        problem = f"{side.relative_to(docs.parent)} is missing"
    else:
        lines = side.read_text(encoding="utf-8").splitlines()
        if not lines or lines[0].strip() != source_hash(docs):
            problem = f"the manual changed after {pdf.relative_to(docs.parent)} was built"
        elif len(lines) != 2:
            problem = (f"{side.relative_to(docs.parent)} holds no digest of the PDF; "
                       "the PDF must be built again")
        elif lines[1].strip() != pdf_hash(docs):
            problem = f"{pdf.relative_to(docs.parent)} is not the PDF that {side.relative_to(docs.parent)} names"
    if problem:
        print(f"manual PDF: {problem}; rebuild it with: {REBUILD}")
        return 1
    print(f"manual PDF: fresh, {pdf.stat().st_size} bytes")
    return 0


def policy() -> str:
    """The value of const Policy in internal/ui/ui.go, joined from its quoted parts."""
    text = (ROOT / "internal" / "ui" / "ui.go").read_text(encoding="utf-8")
    found = re.search(r'^const Policy = ((?:"[^"]*"\s*\+?\s*)+)', text, re.MULTILINE)
    if found is None:
        raise SystemExit("internal/ui/ui.go holds no const Policy")
    return "".join(re.findall(r'"([^"]*)"', found.group(1)))


class Handler(http.server.SimpleHTTPRequestHandler):
    csp = ""

    def end_headers(self) -> None:
        self.send_header("Content-Security-Policy", self.csp)
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Cache-Control", "no-cache")
        super().end_headers()

    def log_message(self, format, *args) -> None:  # noqa: A002
        pass


def start_server() -> tuple[http.server.ThreadingHTTPServer, int]:
    Handler.csp = policy()
    Handler.extensions_map = {**Handler.extensions_map, ".js": "text/javascript", ".svg": "image/svg+xml",
                              ".json": "application/json"}
    ports = list(PORTS)
    random.shuffle(ports)
    for port in ports:
        try:
            server = http.server.ThreadingHTTPServer(("127.0.0.1", port), partial(Handler, directory=str(DOCS)))
        except OSError:
            continue
        threading.Thread(target=server.serve_forever, daemon=True).start()
        return server, port
    raise SystemExit(f"no free port in {PORTS.start} to {PORTS.stop - 1}")


VIOLATIONS_JS = """
window.__violations = [];
document.addEventListener("securitypolicyviolation", (e) => window.__violations.push(e.violatedDirective + " " + e.blockedURI));
"""
HEADINGS_JS = """
() => [...document.querySelectorAll("main h2, main h3")].filter((h) => h.querySelector(":scope > span.number")).map((h) => {
  const number = h.querySelector(":scope > span.number").textContent.trim();
  const id = h.tagName === "H2" ? h.parentElement.id : h.id;
  const title = h.textContent.trim().slice(number.length).trim();
  return {id, number, title};
})
"""
CONTENTS_JS = """
() => Object.fromEntries([...document.querySelectorAll(".print-contents li")].map((li) =>
  [li.querySelector("a").getAttribute("href").slice(1), li.querySelector(".print-page").textContent.trim()]))
"""
WAIT_JS = """
([figures, rows]) => figures.every((name) => {
  const img = document.querySelector(`figure img[src="assets/${name}-light.svg"]`);
  return img && img.complete && img.naturalWidth > 0;
}) && document.querySelectorAll("table[data-openapi] tbody tr").length === rows
  && [...document.images].every((img) => img.complete)
"""


def served_operations() -> int:
    import json
    spec = json.loads((DOCS / "openapi.json").read_text(encoding="utf-8"))
    return sum(1 for item in spec["paths"].values() for op in item.values() if not op.get("x-pipesmith-refused"))


def page_texts(data: bytes) -> list[str]:
    from pypdf import PdfReader
    return [page.extract_text() or "" for page in PdfReader(io.BytesIO(data)).pages]


def find_pages(data: bytes, headings: list[dict]) -> tuple[dict[str, int], str]:
    """The page of every heading id, and the way it was found."""
    from pypdf import PdfReader
    reader = PdfReader(io.BytesIO(data))
    named = {}
    for name, destination in reader.named_destinations.items():
        try:
            named[str(name).lstrip("/")] = reader.get_destination_page_number(destination) + 1
        except Exception:
            continue
    pages, ways = {}, set()
    texts = None
    start = 0
    for head in headings:
        if head["id"] in named:
            pages[head["id"]] = named[head["id"]]
            ways.add("named destinations")
            continue
        if texts is None:
            texts = [[line.strip() for line in text.splitlines()] for text in page_texts(data)]
            first = f"{headings[0]['number']} {headings[0]['title']}"
            hits = [i for i, lines in enumerate(texts) if any(line.startswith(first) for line in lines)]
            start = hits[-1] if hits else 0
        want = f"{head['number']} {head['title']}"
        found = next((i for i in range(start, len(texts)) if any(line.startswith(want) for line in texts[i])), None)
        if found is None:
            raise SystemExit(f"manual PDF: the heading {want!r} (#{head['id']}) is on no page")
        pages[head["id"]] = found + 1
        start = found
        ways.add("page text")
    missing = [head["id"] for head in headings if head["id"] not in pages]
    if missing:
        raise SystemExit(f"manual PDF: no page for the headings {missing}")
    return pages, " and ".join(sorted(ways))


def fixed_dates(data: bytes) -> bytes:
    """The dates of the info dictionary and of an XMP stream replaced by a fixed date of the same length."""
    def same_length(match: re.Match) -> bytes:
        digits = iter(FIXED_DATE)
        text = match.group(0).decode("latin-1")
        return "".join(next(digits, "0") if ch.isdigit() else ch for ch in text).encode("latin-1")
    data = re.sub(rb"(?<=/CreationDate \()D:[0-9+\-Z']+", same_length, data)
    data = re.sub(rb"(?<=/ModDate \()D:[0-9+\-Z']+", same_length, data)
    data = re.sub(rb"(?<=<xmp:CreateDate>)[0-9T:+\-Z.]+", same_length, data)
    data = re.sub(rb"(?<=<xmp:ModifyDate>)[0-9T:+\-Z.]+", same_length, data)
    data = re.sub(rb"(?<=<xmp:MetadataDate>)[0-9T:+\-Z.]+", same_length, data)
    return data


def render(page, version: str) -> bytes:
    header = ('<div style="width:100%;font-size:8px;color:#46526E;text-align:center;font-family:Helvetica,Arial,sans-serif">'
              f"Pipesmith Registry Manual, version {version}</div>")
    footer = ('<div style="width:100%;font-size:8px;color:#46526E;text-align:center;font-family:Helvetica,Arial,sans-serif">'
              'page <span class="pageNumber"></span> of <span class="totalPages"></span></div>')
    return page.pdf(format="A4", print_background=True, display_header_footer=True, header_template=header,
                    footer_template=footer, margin={"top": "20mm", "bottom": "18mm", "left": "16mm", "right": "16mm"},
                    outline=True)


def build() -> int:
    from playwright.sync_api import sync_playwright

    problems: list[str] = []
    server, port = start_server()
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(channel="chrome")
            try:
                context = browser.new_context(viewport={"width": 1280, "height": 900}, color_scheme="light")
                context.add_init_script(VIOLATIONS_JS)
                page = context.new_page()
                page.on("console", lambda m: problems.append(f"console error: {m.text}") if m.type == "error" else None)
                page.on("pageerror", lambda e: problems.append(f"page error: {e}"))
                page.on("requestfailed", lambda r: problems.append(f"request failed: {r.url} ({r.failure})"))
                page.on("response", lambda r: problems.append(f"{r.status} for {r.url}") if r.status >= 400 else None)
                page.goto(f"http://localhost:{port}/index.html")
                page.wait_for_function(WAIT_JS, arg=[list(FIGURES), served_operations()], timeout=WAIT_MS)
                page.wait_for_load_state("networkidle")
                page.emulate_media(media="print")
                version = page.evaluate("(document.querySelector('header.top .version').textContent.match(/(\\d[\\w.+-]*)\\s*$/) || [])[1]")
                if not version:
                    raise SystemExit("manual PDF: the header of index.html names no version")

                page.evaluate("window.pipesmithManual.preparePrint()")
                headings = page.evaluate(HEADINGS_JS)
                first = render(page, version)
                pages, way = find_pages(first, headings)
                page.evaluate("(pages) => window.pipesmithManual.preparePrint(pages)", pages)
                listed = page.evaluate(CONTENTS_JS)
                second = render(page, version)
                problems += [f"securitypolicyviolation {v}" for v in page.evaluate("window.__violations || []")]
            finally:
                browser.close()
    finally:
        server.shutdown()
        server.server_close()
    if problems:
        for line in problems:
            print(f"manual PDF: {line}")
        return 1

    from pypdf import PdfReader
    count1, count2 = len(PdfReader(io.BytesIO(first)).pages), len(PdfReader(io.BytesIO(second)).pages)
    if count1 != count2:
        print(f"manual PDF: pass 1 has {count1} pages, pass 2 has {count2}")
        return 1
    after, _ = find_pages(second, headings)
    wrong = [f"#{key}: contents {listed.get(key)!r}, heading on page {value}" for key, value in after.items()
             if listed.get(key) != str(value)]
    if wrong:
        print("manual PDF: the contents of pass 2 do not match the pages: " + "; ".join(wrong[:5]))
        return 1

    data = fixed_dates(second)
    if len(data) != len(second) or page_texts(data) != page_texts(second):
        print("manual PDF: replacing the dates changed the file beyond the dates")
        return 1
    PDF.parent.mkdir(parents=True, exist_ok=True)
    PDF.write_bytes(data)
    write_source()
    print(f"manual PDF: {PDF.relative_to(ROOT)}, {count2} pages, {len(data)} bytes, "
          f"{len(headings)} headings found by {way}")
    return 0


def render_pages(out: Path) -> int:
    if not PDF.is_file():
        print(f"manual PDF: {PDF.relative_to(ROOT)} is missing; rebuild it with: {REBUILD}")
        return 1
    out.mkdir(parents=True, exist_ok=True)
    for old in out.glob("page-*.png"):
        old.unlink()
    tool = shutil.which("pdftoppm")
    if tool:
        subprocess.run([tool, "-r", "100", "-png", str(PDF), str(out / "page")], check=True)
        used = "pdftoppm"
    else:
        try:
            import pypdfium2
        except ImportError:
            print("manual PDF: no pdftoppm on PATH and no pypdfium2; run with uv run --no-project --with pypdfium2")
            return 1
        document = pypdfium2.PdfDocument(str(PDF))
        for index in range(len(document)):
            document[index].render(scale=100 / 72).to_pil().save(out / f"page-{index + 1:02d}.png")
        used = "pypdfium2"
    print(f"manual PDF: {len(list(out.glob('page-*.png')))} pages rendered by {used} into {out}")
    return 0


def main(argv: list[str]) -> int:
    if argv == []:
        return build()
    if argv == ["--check"]:
        return check()
    if len(argv) == 2 and argv[0] == "--pages":
        return render_pages(Path(argv[1]).resolve())
    print(__doc__)
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
