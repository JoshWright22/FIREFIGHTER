"""Render the brand HTML sources to PNG with headless Chromium (Playwright).

    py -3.12 assets/brand/render.py
"""
from pathlib import Path

from playwright.sync_api import sync_playwright

HERE = Path(__file__).resolve().parent
JOBS = [("social.html", HERE.parent / "social-preview.png", 1280, 640)]

with sync_playwright() as p:
    browser = p.chromium.launch()
    for src, out, w, h in JOBS:
        page = browser.new_page(viewport={"width": w, "height": h}, device_scale_factor=1)
        page.goto((HERE / src).as_uri())
        page.wait_for_timeout(300)
        page.screenshot(path=str(out))
        print("wrote", out)
    browser.close()
