#!/usr/bin/env bash
set -euo pipefail
npm run build
test -f dist/index.html
npx --no-install playwright install --with-deps chromium
CHROME_BIN="$(node --input-type=module -e "import { chromium } from '@playwright/test'; process.stdout.write(chromium.executablePath())")"
export CHROME_BIN
"$CHROME_BIN" --version
npm test
