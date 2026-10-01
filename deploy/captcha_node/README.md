# ZCode plan-channel verification solver

The ZCode plan gateway (`zcode.z.ai/api/v1/zcode-plan/anthropic`) rejects an
OAuth chat request that does not carry a fresh `X-Aliyun-Captcha-Verify-Param`
with HTTP 405 / `{"code":3012,"msg":"request has been blocked due to unusual
activity."}`. The token is minted by Aliyun's traceless-verification service for
a browser session its risk engine trusts, so it cannot be computed locally.

`solver.js` runs the official Aliyun SDK in a real Chromium (puppeteer-core +
the system browser) and prints `VERIFY_PARAM=<token>` on success. A simulated DOM
and a browser whose platform signals contradict its user agent are both scored
as bots — the platform override is what makes this pass.

The gateway calls it out of process; `internal/providers/zcode/captcha.go` keeps
a small prefetch pool so the chat path never waits on a browser launch.

Deployment: the solver directory is mounted or copied to `ZCODE_CAPTCHA_DIR`
(default `/app/captcha_node`), which must contain `solver.js`, `page.html` and
`node_modules` (`npm install` in this directory). Chromium is expected at
`/usr/bin/chromium` unless `ZCODE_CHROMIUM_PATH` says otherwise.
