const https = require("https");
const fs = require("fs");
const path = require("path");
const { execSync } = require("child_process");
const os = require("os");

const PLATFORMS = {
  "darwin-arm64": { archive: "quest-darwin-arm64.tar.gz", binary: "quest" },
  "darwin-x64": { archive: "quest-darwin-x64.tar.gz", binary: "quest" },
  "linux-arm64": { archive: "quest-linux-arm64.tar.gz", binary: "quest" },
  "linux-x64": { archive: "quest-linux-x64.tar.gz", binary: "quest" },
  "win32-x64": { archive: "quest-win32-x64.zip", binary: "quest.exe" },
};

function getPackageVersion() {
  const pkg = JSON.parse(
    fs.readFileSync(path.join(__dirname, "..", "package.json"), "utf8")
  );
  return pkg.version;
}

function getPlatformKey() {
  const platform = process.platform;
  const arch = process.arch;
  return `${platform}-${arch}`;
}

function download(url) {
  return new Promise((resolve, reject) => {
    https
      .get(url, (res) => {
        if (res.statusCode === 301 || res.statusCode === 302) {
          return download(res.headers.location).then(resolve).catch(reject);
        }
        if (res.statusCode !== 200) {
          return reject(
            new Error(`Download failed: HTTP ${res.statusCode} from ${url}`)
          );
        }
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () => resolve(Buffer.concat(chunks)));
        res.on("error", reject);
      })
      .on("error", reject);
  });
}

async function install() {
  if (process.env.QUEST_BINARY_PATH) {
    const src = process.env.QUEST_BINARY_PATH;
    const vendorDir = path.join(__dirname, "..", "vendor");
    const binaryName = process.platform === "win32" ? "quest.exe" : "quest";
    const dest = path.join(vendorDir, binaryName);
    fs.mkdirSync(vendorDir, { recursive: true });
    fs.copyFileSync(src, dest);
    fs.chmodSync(dest, 0o755);
    console.log(`quest: using local binary from ${src}`);
    return;
  }

  const key = getPlatformKey();
  const info = PLATFORMS[key];
  if (!info) {
    console.error(
      `quest: unsupported platform ${key}. Supported: ${Object.keys(PLATFORMS).join(", ")}`
    );
    process.exit(1);
  }

  const version = getPackageVersion();
  const url = `https://github.com/alefemafra/quest-cli/releases/download/v${version}/${info.archive}`;

  console.log(`quest: downloading ${info.archive} (v${version})...`);

  let data;
  try {
    data = await download(url);
  } catch (err) {
    console.error(`quest: failed to download binary — ${err.message}`);
    console.error(`quest: url was ${url}`);
    console.error(
      `quest: you can set QUEST_BINARY_PATH to point to a local binary`
    );
    process.exit(1);
  }

  const vendorDir = path.join(__dirname, "..", "vendor");
  fs.mkdirSync(vendorDir, { recursive: true });

  const tmpFile = path.join(os.tmpdir(), info.archive);
  fs.writeFileSync(tmpFile, data);

  if (info.archive.endsWith(".zip")) {
    execSync(`unzip -o "${tmpFile}" -d "${vendorDir}"`, { stdio: "pipe" });
  } else {
    execSync(`tar xzf "${tmpFile}" -C "${vendorDir}"`, { stdio: "pipe" });
  }

  fs.unlinkSync(tmpFile);

  const binaryPath = path.join(vendorDir, info.binary);
  if (process.platform !== "win32") {
    fs.chmodSync(binaryPath, 0o755);
  }

  console.log(`quest: installed successfully (v${version})`);
}

install().catch((err) => {
  console.error(`quest: installation failed — ${err.message}`);
  process.exit(1);
});
