// MCP Bundles (.mcpb) for the MCP Registry, one per OS/arch that goreleaser builds (a manifest can
// override the command per OS but not per arch).
//
//   node integrations/mcpb/build.mjs stage <version> [dist]     dist/mcpb/firefighter_<os>_<arch>/ from dist/artifacts.json
//   node integrations/mcpb/build.mjs server-json <version> <dir>  version, URLs and sha256 of <dir>/*.mcpb into server.json
//
// The release workflow packs each staged folder with the pinned mcpb CLI. No dependencies: plain Node.
import { createHash } from "node:crypto";
import { chmodSync, copyFileSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const REPO = "https://github.com/JoshWright22/FIREFIGHTER";
const PLATFORM = { windows: "win32", darwin: "darwin", linux: "linux" }; // goos -> process.platform

const [cmd, version, dir] = process.argv.slice(2);
if (!/^\d+\.\d+\.\d+([-+].*)?$/.test(version ?? "")) {
  console.error("usage: build.mjs stage|server-json <semver without v> [dir]");
  process.exit(2);
}

function manifest(goos, exe) {
  return {
    manifest_version: "0.3",
    name: "firefighter",
    display_name: "FIREFIGHTER",
    version,
    description: "The coordinator and scheduler for unmanaged AI agents sharing one computer.",
    long_description:
      "Agents queue heavy work (training, evals, builds, renders, docker jobs) with firefighter_submit and " +
      "firefighter_wait instead of colliding on the GPU, VRAM, CPU or RAM, and jobs pause or checkpoint when " +
      "you need the machine back. The bundle starts the local FIREFIGHTER daemon on first use. For start at " +
      "logon, the `firefighter` CLI on your PATH and rules for your other agents, install it too: see " +
      `${REPO}#install-one-command.`,
    author: { name: "Josh Wright", url: "https://github.com/JoshWright22" },
    repository: { type: "git", url: `${REPO}.git` },
    homepage: "https://joshwright22.github.io/FIREFIGHTER/",
    documentation: `${REPO}#readme`,
    support: `${REPO}/issues`,
    icon: "icon.png",
    server: {
      type: "binary",
      entry_point: `server/${exe}`,
      mcp_config: { command: `\${__dirname}/server/${exe}`, args: ["mcp"] },
    },
    tools: [
      { name: "firefighter_submit", description: "Queue heavy work (GPU, or CPU over ~1 min) on this shared machine" },
      { name: "firefighter_wait", description: "Wait up to ~45 s on a job; receipt and log tail when done" },
      { name: "firefighter_status", description: "The machine, the queue, budgets and price quotes" },
      { name: "firefighter_job", description: "Extend, re-prioritize or cancel a job" },
      { name: "firefighter_plan", description: "Announce heavy work for later so others plan around it" },
    ],
    tools_generated: true, // firefighter_config is listed only when the human allows it
    keywords: ["gpu", "vram", "scheduler", "job-queue", "agents", "local"],
    license: "MIT",
    compatibility: { platforms: [PLATFORM[goos]] },
  };
}

function stage(dist = join(ROOT, "dist")) {
  const out = join(dist, "mcpb");
  rmSync(out, { recursive: true, force: true });
  const bins = JSON.parse(readFileSync(join(dist, "artifacts.json"), "utf8")).filter(
    (a) => a.type === "Binary" && PLATFORM[a.goos] && ["amd64", "arm64"].includes(a.goarch),
  );
  if (bins.length === 0) throw new Error("no binaries in artifacts.json");
  for (const a of bins) {
    const exe = a.goos === "windows" ? "firefighter.exe" : "firefighter";
    const d = join(out, `firefighter_${a.goos}_${a.goarch}`);
    mkdirSync(join(d, "server"), { recursive: true });
    copyFileSync(resolve(ROOT, a.path), join(d, "server", exe));
    chmodSync(join(d, "server", exe), 0o755);
    copyFileSync(join(ROOT, "assets", "icon-512.png"), join(d, "icon.png"));
    copyFileSync(join(ROOT, "LICENSE"), join(d, "LICENSE"));
    writeFileSync(join(d, "manifest.json"), JSON.stringify(manifest(a.goos, exe), null, 2) + "\n");
    console.log("staged", d);
  }
}

function serverJson(bundles) {
  const files = readdirSync(bundles).filter((f) => /^firefighter_[a-z]+_[a-z0-9]+\.mcpb$/.test(f)).sort();
  if (files.length === 0) throw new Error(`no .mcpb files in ${bundles}`);
  const path = join(ROOT, "server.json");
  const s = JSON.parse(readFileSync(path, "utf8"));
  s.version = version;
  s.packages = files.map((f) => ({
    registryType: "mcpb",
    identifier: `${REPO}/releases/download/v${version}/${f}`,
    fileSha256: createHash("sha256").update(readFileSync(join(bundles, f))).digest("hex"),
    transport: { type: "stdio" },
  }));
  writeFileSync(path, JSON.stringify(s, null, 2) + "\n");
  console.log(`server.json: ${version}, ${files.length} packages`);
}

if (cmd === "stage") stage(dir && resolve(dir));
else if (cmd === "server-json" && dir && existsSync(dir)) serverJson(resolve(dir));
else {
  console.error("usage: build.mjs stage|server-json <semver without v> [dir]");
  process.exit(2);
}
