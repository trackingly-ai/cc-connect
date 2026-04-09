---
name: chromium-extension-debug-playwright
description: Use when debugging a Chrome extension with Playwright on macOS and you need the reliable Chromium + CDP workflow instead of Chrome or Chrome for Testing. Covers installing open-source Chromium, finding its binary, launching it from the command line with a persistent profile and unpacked extension, then attaching Playwright via connectOverCDP to inspect pages, popup/options UIs, and native-host wiring.
---

# Chromium Extension Debug With Playwright

Use this skill for local macOS debugging of browser extensions when:
- the extension must run in a real browser UI
- Playwright is needed for page automation or console/log capture
- the workflow must use **open-source Chromium**
- the extension depends on a persistent profile, unpacked loading, or native messaging

Do **not** use Google Chrome or Chrome for Testing for this workflow unless the user explicitly asks for them.

## Core rule

For extension debugging, launch **Chromium** yourself from the command line, then attach Playwright with `chromium.connectOverCDP(...)`.

Do not rely on a fresh `browserType.launch()` flow for this use case.

## Install Chromium on macOS

If Chromium is not installed:

```bash
brew install --cask chromium
```

Typical app path:

```bash
/Applications/Chromium.app/Contents/MacOS/Chromium
```

## Find the Chromium binary

Try these in order:

```bash
find /Applications ~/Applications ~ -path '*/Chromium.app/Contents/MacOS/Chromium' 2>/dev/null
```

```bash
mdfind 'kMDItemDisplayName == "Chromium"'
```

If installed via Homebrew cask, prefer:

```bash
/Applications/Chromium.app/Contents/MacOS/Chromium
```

## Launch Chromium for extension debugging

Use a persistent profile and load the unpacked extension explicitly:

```bash
"/Applications/Chromium.app/Contents/MacOS/Chromium" \
  --user-data-dir=/tmp/mysterious-chromium-profile \
  --remote-debugging-port=9224 \
  --disable-extensions-except=/ABS/PATH/TO/EXTENSION \
  --load-extension=/ABS/PATH/TO/EXTENSION \
  about:blank
```

Required pieces:
- `--user-data-dir=...`: keeps extension state and makes the session attachable
- `--remote-debugging-port=...`: needed for CDP
- `--disable-extensions-except=...`: limits extensions to the target one
- `--load-extension=...`: loads the unpacked extension

## Attach Playwright over CDP

From Node:

```js
const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.connectOverCDP('http://127.0.0.1:9224');
  const context = browser.contexts()[0];
  const page = await context.newPage();
  await page.goto('https://example.com', { waitUntil: 'domcontentloaded' });
})();
```

Important:
- use `connectOverCDP`
- attach to the already-running Chromium instance
- reuse the first context from the persistent profile

## Native messaging note

If the extension talks to a native host and Chromium is launched with a custom `--user-data-dir`, verify the host manifest is visible to the Chromium profile you launched.

Check both:
- the normal Chromium user host directory:

```bash
~/Library/Application Support/Chromium/NativeMessagingHosts
```

- and, if the debug setup uses a dedicated profile-specific host location or copied manifests, make sure the launched profile can actually see the same `com.example.host.json`

At minimum, inspect the active manifest:

```bash
cat ~/Library/Application\ Support/Chromium/NativeMessagingHosts/com.mysterious.companion.json
```

If the extension says `host not found`, first verify:
- the manifest file exists
- `allowed_origins` contains the real extension origin
- `path` points to an executable binary

## Extension staging checklist

Before launch:
- rebuild or restage the unpacked extension if the UI changed
- confirm `manifest.json` is the expected one
- if a stable ID is required, make sure `manifest.key` is present in the staged copy

Quick checks:

```bash
cat /ABS/PATH/TO/EXTENSION/manifest.json
```

```bash
ls -la /ABS/PATH/TO/EXTENSION
```

## Useful debug flow

1. Find the Chromium binary.
2. Launch Chromium from the command line with `--user-data-dir`, `--remote-debugging-port`, `--disable-extensions-except`, and `--load-extension`.
3. Open `chrome://extensions` in that Chromium session.
4. Confirm the extension is loaded and inspect the service worker if needed.
5. Attach Playwright with `chromium.connectOverCDP(...)`.
6. Open the target site and reproduce the extension action.
7. If native messaging is involved, verify host manifest path and `allowed_origins` before blaming Playwright.

## Common failure patterns

- `connectOverCDP` fails:
  - Chromium was not launched with `--remote-debugging-port`
  - wrong port
  - browser process already exited

- Extension not loaded:
  - wrong unpacked path
  - missing `--load-extension`
  - loaded a stale staged directory

- Extension loads but native host is unavailable:
  - wrong native host manifest path
  - wrong `allowed_origins`
  - host binary path invalid
  - host binary not executable

- Wrong browser:
  - Playwright cache paths often point to bundled browsers; for this workflow, explicitly use the system-installed open-source Chromium binary

## Minimal command set

Find Chromium:

```bash
find /Applications ~/Applications ~ -path '*/Chromium.app/Contents/MacOS/Chromium' 2>/dev/null
```

Launch Chromium:

```bash
"/Applications/Chromium.app/Contents/MacOS/Chromium" \
  --user-data-dir=/tmp/chromium-extension-debug \
  --remote-debugging-port=9224 \
  --disable-extensions-except=/ABS/PATH/TO/EXTENSION \
  --load-extension=/ABS/PATH/TO/EXTENSION \
  about:blank
```

Attach Playwright:

```bash
node -e "const { chromium } = require('playwright'); (async()=>{ const browser = await chromium.connectOverCDP('http://127.0.0.1:9224'); console.log(browser.contexts().length); })()"
```
