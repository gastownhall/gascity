# Gas City Development

## Browser Automation (Chrome CDP)

When a task requires browser interaction — configuring third-party portals, UAT against
web UIs, scraping authenticated pages — use Playwright with Chrome DevTools Protocol
(CDP) instead of asking the user for manual steps.

**Prerequisite:** Chrome must be running with remote debugging enabled. On Linux,
`--user-data-dir` must be a non-default path:

```bash
google-chrome --remote-debugging-port=9222 --user-data-dir=/tmp/chrome-debug
```

**Connecting from a script:**

```python
from playwright.sync_api import sync_playwright

with sync_playwright() as p:
    browser = p.chromium.connect_over_cdp("http://localhost:9222")
    page = browser.contexts[0].pages[0]
    # ... interact with the page
```

**Division of labor:** The user handles login and 2FA in the Chrome session. The agent
does everything after authentication is complete.

**Debugging:** Take a screenshot at each step so failures are diagnosable:

```python
page.screenshot(path="step-01-loaded.png")
```

**Dependency:** Add `playwright` as a dev dependency (`pip install --dev playwright` or
equivalent for your project).
