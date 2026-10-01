#!/usr/bin/env node
//
// Aliyun traceless-verification token for the ZCode plan channel.
//
// The plan gateway refuses an OAuth chat request that does not carry a fresh
// X-Aliyun-Captcha-Verify-Param, and Aliyun only issues one to a browser its
// risk engine trusts. A simulated DOM or a headless browser whose platform
// signals contradict its user agent is scored as a bot and answered with
// verifyCode F001 ("interactive verification required"), so this drives a real
// Chromium through the official SDK on a plain local page, with the whole
// platform surface overridden to a Windows desktop.
//
// usage: node solver.js <sceneId> <region> <prefix>
// stdout: VERIFY_PARAM=<token>
// exit:   0 solved, 2 bad args, 3 missing chromium/puppeteer, 4 no token, 5 error

"use strict";

const fs = require("node:fs");
const path = require("node:path");

const [sceneId, region, prefix] = process.argv.slice(2);
if (!sceneId) {
  process.stderr.write("usage: solver.js <sceneId> <region> <prefix>\n");
  process.exit(2);
}

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36";
const PAGE_URL = "file://" + path.join(__dirname, "page.html");
const SOLVE_TIMEOUT_MS = Number(process.env.CAPTCHA_SOLVE_TIMEOUT_MS || 30000);
const LAUNCH_TIMEOUT_MS = Number(process.env.CAPTCHA_LAUNCH_TIMEOUT_MS || 90000);
const MIN_FREE_MB = Number(process.env.CAPTCHA_MIN_FREE_MB || 400);
const ATTEMPTS = Number(process.env.CAPTCHA_SOLVE_ATTEMPTS || 2);
const STARTED_AT = Date.now();

const CHROME_FLAGS = [
  "--no-sandbox",
  "--disable-dev-shm-usage",
  "--disable-blink-features=AutomationControlled",
  "--disable-features=site-per-process",
  "--lang=zh-CN",
  "--single-process",
  "--no-zygote",
  "--renderer-process-limit=1",
  "--disable-gpu",
  "--disable-software-rasterizer",
  "--disable-extensions",
  "--disable-background-networking",
  "--disable-default-apps",
  "--disable-component-update",
  "--disable-sync",
  "--no-first-run",
  "--memory-pressure-off",
  "--disable-background-timer-throttling",
  "--disable-renderer-backgrounding",
  "--disable-backgrounding-occluded-windows",
];

function log(message) {
  if (/^(1|true|yes)$/i.test(process.env.CAPTCHA_DEBUG || "")) {
    process.stderr.write("[captcha] " + message + "\n");
  }
}

function freeMemoryMB() {
  try {
    const line = fs
      .readFileSync("/proc/meminfo", "utf8")
      .split("\n")
      .find((entry) => entry.startsWith("MemAvailable:"));
    return line ? parseInt(line.replace(/[^0-9]/g, ""), 10) : Infinity;
  } catch {
    return Infinity; // not Linux: no gate
  }
}

function findChromium() {
  const candidates = [
    process.env.ZCODE_CHROMIUM_PATH,
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/usr/bin/google-chrome",
    "/usr/local/bin/chromium",
    "/usr/local/bin/google-chrome",
  ].filter(Boolean);
  for (const candidate of candidates) {
    try {
      fs.accessSync(candidate, fs.constants.X_OK);
      return candidate;
    } catch {
      /* keep looking */
    }
  }
  return null;
}

// Chromium raises its own oom_score_adj to 800 on Linux, which makes it the
// first casualty of memory pressure on a shared box. Pin it back to 0 after
// launch so the kernel picks a real offender instead.
function pinOomScore(pid) {
  try {
    fs.writeFileSync(`/proc/${pid}/oom_score_adj`, "0");
  } catch {
    /* best effort */
  }
}

let puppeteer;
try {
  puppeteer = require("puppeteer-core");
} catch (error) {
  process.stderr.write(
    "[captcha] puppeteer-core is missing: run npm install in " + __dirname + "\n",
  );
  process.exit(3);
}

async function mint(executablePath) {
  const started = Date.now();
  const proxy = process.env.HTTPS_PROXY || process.env.HTTP_PROXY || "";
  const args = proxy ? CHROME_FLAGS.concat([`--proxy-server=${proxy}`]) : CHROME_FLAGS;
  const browser = await puppeteer.launch({
    executablePath,
    headless: true,
    args,
    defaultViewport: { width: 1280, height: 720 },
    protocolTimeout: 120000,
    timeout: LAUNCH_TIMEOUT_MS,
  });
  log(`launched in ${Date.now() - started}ms`);
  const child = browser.process();
  if (child && child.pid) pinOomScore(child.pid);
  try {
    const page = await browser.newPage();
    await page.setUserAgent(USER_AGENT, {
      architecture: "x86",
      bitness: "64",
      mobile: false,
      model: "",
      platform: "Windows",
      platformVersion: "10.0.0",
      wow64: false,
    });
    await page.evaluateOnNewDocument(() => {
      Object.defineProperty(navigator, "webdriver", { get: () => undefined });
      Object.defineProperty(navigator, "languages", { get: () => ["zh-CN", "zh", "en"] });
      Object.defineProperty(navigator, "platform", { get: () => "Win32" });
    });
    await page.goto(PAGE_URL, { waitUntil: "networkidle2", timeout: 30000 });
    await page.waitForFunction("typeof window.initAliyunCaptcha === 'function'", {
      timeout: 20000,
    });
    log(`sdk ready in ${Date.now() - started}ms`);
    return await page.evaluate(
      async (scene, regionName, prefixName, timeoutMs) => {
        window.AliyunCaptchaConfig = { region: regionName, prefix: prefixName };
        return await new Promise((resolve) => {
          let settled = false;
          const finish = (value) => {
            if (!settled) {
              settled = true;
              resolve(value || null);
            }
          };
          try {
            window.initAliyunCaptcha({
              SceneId: scene,
              mode: "popup",
              language: "zh-CN",
              showErrorTip: false,
              element: "#cap-holder",
              button: "#cap-btn",
              getInstance: (instance) => {
                try {
                  instance.startTracelessVerification();
                } catch {
                  finish(null);
                }
              },
              success: (result) => {
                const token =
                  typeof result === "string" ? result : result && result.captchaVerifyParam;
                finish(token);
              },
              fail: () => finish(null),
              onError: () => finish(null),
            });
          } catch {
            finish(null);
          }
          setTimeout(() => finish(null), timeoutMs);
        });
      },
      sceneId,
      region,
      prefix,
      SOLVE_TIMEOUT_MS,
    );
  } finally {
    try {
      await browser.close();
    } catch {
      /* the exit code already carries the outcome */
    }
  }
}

(async () => {
  try {
    fs.writeFileSync(`/proc/${process.pid}/oom_score_adj`, "0");
  } catch {
    /* best effort */
  }
  const executablePath = findChromium();
  if (!executablePath) {
    process.stderr.write(
      "[captcha] no Chromium found; install it or set ZCODE_CHROMIUM_PATH\n",
    );
    process.exit(3);
  }
  for (let attempt = 1; attempt <= ATTEMPTS; attempt++) {
    if (freeMemoryMB() < MIN_FREE_MB) {
      process.stderr.write(
        `[captcha] only ${Math.round(freeMemoryMB())}MB free, waiting before attempt ${attempt}\n`,
      );
      await new Promise((resolve) => setTimeout(resolve, 5000));
      continue;
    }
    try {
      const token = await mint(executablePath);
      if (token && token.trim()) {
        log(`solved in ${Date.now() - STARTED_AT}ms`);
        process.stdout.write("VERIFY_PARAM=" + token.trim() + "\n");
        process.exit(0);
      }
      log(`attempt ${attempt}: no token`);
    } catch (error) {
      log(`attempt ${attempt}: ${(error && error.message) || error}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 2000));
  }
  process.stderr.write("[captcha] no verification token after retries\n");
  process.exit(4);
})().catch((error) => {
  process.stderr.write("[captcha] fatal: " + ((error && error.message) || error) + "\n");
  process.exit(5);
});
