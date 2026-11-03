"""Build every brand asset from one place.

    py -3.12 assets/brand/build.py

Writes assets/logo.svg and assets/icon.svg with the lettering converted to paths (GitHub renders SVGs
without web fonts), then renders the PNGs with headless Chromium (Playwright): the 512 px icons and the
1280x640 social preview. Copies what the Pages site uses into docs/.

Look: crimson poster, the name repeated in Anton, one line lit in ivory. Colors come from frames of the
K/DA "FIRESTARTER" video (near-black, crimson, wine, candle ivory); nothing else is borrowed from it.
"""
import shutil
from pathlib import Path

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from playwright.sync_api import sync_playwright

HERE = Path(__file__).resolve().parent
ASSETS = HERE.parent
ROOT = ASSETS.parent

INK, CRIMSON, IVORY = "#0f0f0f", "#ad004f", "#f3e6cf"

font = TTFont(HERE / "fonts" / "Anton-Regular.ttf")
glyphs, cmap, hmtx = font.getGlyphSet(), font.getBestCmap(), font["hmtx"]
UPM = font["head"].unitsPerEm


def text_path(s, x, y, size, track=0.0):
    """SVG path data for s in Anton, baseline at (x, y), size in px, track in em."""
    k = size / UPM
    pen = SVGPathPen(glyphs)
    for ch in s:
        name = cmap[ord(ch)]
        glyphs[name].draw(TransformPen(pen, (k, 0, 0, -k, x, y)))
        x += hmtx[name][0] * k + track * size
    return pen.getCommands()


def width(s, size, track=0.0):
    return sum(hmtx[cmap[ord(c)]][0] for c in s) * size / UPM + track * size * len(s)


def poster(w, h, rows, size, pitch, lit, word, x0, y0, stroke, lit_word=None, lit_x=None):
    """Rows of the word; row `lit` is solid ivory, the rest alternate solid ink and outlined ink."""
    lit_word = lit_word or word
    lit_x = x0 if lit_x is None else lit_x
    out = [f'<rect width="{w}" height="{h}" fill="{CRIMSON}"/>']
    for i in range(rows):
        # The lit row starts on a whole word; the others run offset like a repeated poster.
        x = lit_x if i == lit else x0 - (i % 2) * size * 0.9
        d = text_path(lit_word if i == lit else word, x, y0 + i * pitch, size)
        if i == lit:
            out.append(f'<path d="{d}" fill="{IVORY}"/>')
            rest = x + width(lit_word + " ", size)
            if rest < w - 0.6 * size:  # keep the row running in ink after the lit word
                out.append(f'<path d="{text_path(word, rest, y0 + i * pitch, size)}" fill="{INK}"/>')
        elif i % 2 == 0:
            out.append(f'<path d="{d}" fill="none" stroke="{INK}" stroke-width="{stroke}"/>')
        else:
            out.append(f'<path d="{d}" fill="{INK}"/>')
    return out


def svg(w, h, label, body):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" role="img" aria-label="{label}">\n'
            f'  <title>{label}</title>\n  ' + "\n  ".join(body) + "\n</svg>\n")


# Wordmark: three rows, the middle one lit.
logo = poster(640, 200, 3, 86, 74, 1, "FIREFIGHTER FIREFIGHTER", -12, 46, 1.4, "FIREFIGHTER", 24)
(ASSETS / "logo.svg").write_text(svg(640, 200, "firefighter", logo), encoding="utf-8")

# Icon: the same poster cropped to a square; one big lit "FF" so it still reads at 16 px.
icon = poster(256, 256, 3, 168, 150, 1, "FFFFF", -40, 28, 2.6, "FF", (256 - width("FF", 168)) / 2)
(ASSETS / "icon.svg").write_text(svg(256, 256, "firefighter icon", icon), encoding="utf-8")

with sync_playwright() as p:
    browser = p.chromium.launch()
    jobs = [
        (ASSETS / "icon.svg", [ASSETS / "icon-512.png", ROOT / "integrations/vscode/icon.png",
                               ROOT / "integrations/claude-code/.claude-plugin/icon.png"], 512, 512),
        (HERE / "social.html", [ASSETS / "social-preview.png"], 1280, 640),
    ]
    for src, outs, w, h in jobs:
        page = browser.new_page(viewport={"width": w, "height": h})
        page.goto(src.as_uri())  # an SVG without width/height fills the viewport
        page.evaluate("document.fonts.ready")
        page.wait_for_timeout(300)
        page.screenshot(path=str(outs[0]))
        for o in outs[1:]:
            shutil.copyfile(outs[0], o)
        print("wrote", *[o.relative_to(ROOT).as_posix() for o in outs])
    browser.close()

for name in ["icon.svg", "demo.svg", "social-preview.png"]:
    shutil.copyfile(ASSETS / name, ROOT / "docs" / name)
print("copied icon.svg, demo.svg, social-preview.png to docs/")
