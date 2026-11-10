#!/usr/bin/env bash
set -euo pipefail
npm test
npm run build
test -f dist/index.html
