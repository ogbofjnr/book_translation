from pathlib import Path
from playwright.sync_api import sync_playwright


OUT = Path("/home/ogbofjnr/code/book_translation/live_reader_app/storage/debug")
OUT.mkdir(parents=True, exist_ok=True)


def main():
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1440, "height": 1024})
        page.goto("http://localhost:8080", wait_until="networkidle")
        page.wait_for_selector("#booksList button", timeout=10000)
        page.locator("#booksList button").first.click()
        page.wait_for_timeout(3000)

        # Open sidebar toggle cycle to reproduce layout issue.
        page.click("#toggleSidebarBtn")
        page.wait_for_timeout(500)
        page.click("#toggleSidebarBtn")
        page.wait_for_timeout(1200)

        # Collect layout metrics from iframe
        metrics = page.evaluate(
            """
            () => {
              const iframe = document.querySelector('#reader iframe');
              if (!iframe || !iframe.contentDocument || !iframe.contentDocument.body) {
                return { error: 'iframe/body not ready' };
              }
              const body = iframe.contentDocument.body;
              const html = iframe.contentDocument.documentElement;
              const sampleP = body.querySelector('p, div, section');
              const pStyle = sampleP ? iframe.contentWindow.getComputedStyle(sampleP) : null;
              const bodyStyle = iframe.contentWindow.getComputedStyle(body);
              return {
                iframeClientWidth: iframe.clientWidth,
                bodyClientWidth: body.clientWidth,
                htmlClientWidth: html.clientWidth,
                bodyMaxWidth: bodyStyle.maxWidth,
                bodyMargin: bodyStyle.margin,
                sampleTag: sampleP ? sampleP.tagName : null,
                sampleMaxWidth: pStyle ? pStyle.maxWidth : null,
                sampleWidth: sampleP ? sampleP.getBoundingClientRect().width : null
              };
            }
            """
        )
        (OUT / "layout_metrics.txt").write_text(str(metrics), encoding="utf-8")
        page.screenshot(path=str(OUT / "ui_after_toggle.png"), full_page=True)
        browser.close()


if __name__ == "__main__":
    main()
