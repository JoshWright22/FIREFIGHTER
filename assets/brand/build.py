"""Build every brand asset from one place.

    py -3.12 assets/brand/build.py

Writes assets/logo.svg, assets/icon.svg and assets/social-preview.svg with the lettering converted to
paths (GitHub renders SVGs without web fonts), then renders the PNGs with headless Chromium (Playwright):
the 512 px icons and the 1280x640 social preview. Copies what the Pages site uses into docs/.

Look: magenta poster, the name repeated in Inter Tight Black, centered on a cream + black pair; the icon
is "FF" built from bars, with a ring. Fonts are OFL (assets/brand/fonts); the Black weight is instanced
from the variable font in memory, not saved (needs skia-pathops for the overlap merge).
"""
import shutil
from pathlib import Path

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.ttLib.removeOverlaps import removeOverlaps
from fontTools.varLib.instancer import instantiateVariableFont
from playwright.sync_api import sync_playwright

HERE = Path(__file__).resolve().parent
ASSETS = HERE.parent
ROOT = ASSETS.parent

MAGENTA, INK, CREAM = "#d4147a", "#141216", "#fff3e8"
WORD = "FIREFIGHTER"
TRACK = -0.02  # em; Inter Tight Black set tight like a poster

font = instantiateVariableFont(TTFont(HERE / "fonts" / "InterTight.ttf"), {"wght": 900})
removeOverlaps(font)  # variable-font strokes overlap; outlines would show the seams
glyphs, cmap, hmtx = font.getGlyphSet(), font.getBestCmap(), font["hmtx"]
UPM = font["head"].unitsPerEm
CAP = font["OS/2"].sCapHeight / UPM


def text_path(s, x, y, size):
    """SVG path data for s, baseline at (x, y), size in px."""
    k = size / UPM
    pen = SVGPathPen(glyphs)
    for ch in s:
        name = cmap[ord(ch)]
        glyphs[name].draw(TransformPen(pen, (k, 0, 0, -k, x, y)))
        x += hmtx[name][0] * k + TRACK * size
    return pen.getCommands()


def width(s, size):
    return sum(hmtx[cmap[ord(c)]][0] for c in s) * size / UPM + TRACK * size * len(s)


def fit(w, share, gap=0.32):
    """Font size at which the centered pair spans `share` of the width w."""
    return share * w / (2 * width(WORD, 1) + gap)


def poster(w, h, size, rows, gap=0.32, stroke=1.6):
    """Rows of the name across a w x h magenta field, repeating both ways from the center. Every row puts
    the gap between two names at the exact middle; on the middle row those two are solid, cream on the
    left and black on the right, and everything else is outlined."""
    g = gap * size
    step = width(WORD, size) + g
    pitch = size * CAP * 1.42
    mid = rows // 2
    base = h / 2 + size * CAP / 2  # baseline that centers the middle row's caps
    left, right = w / 2 - g / 2 - width(WORD, size), w / 2 + g / 2
    out = [f'<rect width="{w}" height="{h}" fill="{MAGENTA}"/>']
    for r in range(rows):
        y = base + (r - mid) * pitch
        x = right
        while x > -step:
            x -= step
        while x < w:
            d = text_path(WORD, x, y, size)
            if r == mid and abs(x - left) < 1:
                out.append(f'<path d="{d}" fill="{CREAM}"/>')
            elif r == mid and abs(x - right) < 1:
                out.append(f'<path d="{d}" fill="{INK}"/>')
            else:
                out.append(f'<path d="{d}" fill="none" stroke="{INK}" stroke-width="{stroke}"/>')
            x += step
    return out


def icon(S=256):
    """FF from bars, a ring at the foot of the second F."""
    u = S / 10
    bars = [(2, 2, 1.3, 6), (2, 2, 2.8, 1.3), (2, 4.4, 2.2, 1.2),
            (5.5, 2, 1.3, 6), (5.5, 2, 2.8, 1.3), (5.5, 4.4, 2.2, 1.2)]
    out = [f'<rect width="{S}" height="{S}" fill="{MAGENTA}"/>', f'<g fill="{CREAM}">']
    out += [f'<rect x="{x*u:g}" y="{y*u:g}" width="{bw*u:g}" height="{bh*u:g}"/>' for x, y, bw, bh in bars]
    out += ['</g>', f'<circle cx="{8.1*u:g}" cy="{7.1*u:g}" r="{0.75*u:g}" fill="none" stroke="{CREAM}" '
                    f'stroke-width="{0.55*u:g}"/>']
    return out


def svg(w, h, label, body):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" role="img" aria-label="{label}">\n'
            f'  <title>{label}</title>\n  ' + "\n  ".join(body) + "\n</svg>\n")


(ASSETS / "logo.svg").write_text(svg(640, 200, "firefighter", poster(640, 200, fit(640, 0.9), 3)), encoding="utf-8")
(ASSETS / "icon.svg").write_text(svg(256, 256, "firefighter icon", icon()), encoding="utf-8")
(ROOT / "docs" / "poster.svg").write_text(svg(2400, 640, "firefighter", poster(2400, 640, 120, 5, stroke=2)),
                                         encoding="utf-8")  # the landing page banner
(HERE / "social.svg").write_text(svg(1280, 640, "firefighter", poster(1280, 640, fit(1280, 0.86), 5, stroke=2)), encoding="utf-8")

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

# Fonts for `firefighter dashboard`, embedded in the binary: subset to what the page draws.
from fontTools import subset as _subset
DASH = ROOT / "cmd" / "firefighter" / "dash"
_opts = _subset.Options()
_opts.layout_features = ["kern", "liga", "calt", "tnum"]
_basic = "".join(chr(c) for c in range(0x20, 0x7F)) + "·…→←↑↓◆●○–—×%°"
for src, out, f in [("InterTight", "InterTight-Black.ttf", font),
                    ("JetBrainsMono", "JetBrainsMono.ttf", TTFont(HERE / "fonts" / "JetBrainsMono.ttf"))]:
    sub = _subset.Subsetter(_opts)
    sub.populate(text=_basic)
    sub.subset(f)
    f.save(DASH / out)
    shutil.copyfile(HERE / "fonts" / f"OFL-{src}.txt", DASH / f"OFL-{src}.txt")
print("wrote dashboard fonts")

for name in ["icon.svg", "demo.svg", "social-preview.png"]:
    shutil.copyfile(ASSETS / name, ROOT / "docs" / name)
print("copied icon.svg, demo.svg, social-preview.png to docs/")
