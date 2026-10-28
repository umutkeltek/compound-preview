import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync, existsSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const script = join(import.meta.dir, "compound_home_backup.ts");
const temps: string[] = [];

afterEach(() => {
  for (const dir of temps.splice(0)) rmSync(dir, { recursive: true, force: true });
});

function tempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), "compound-home-backup-test-"));
  temps.push(dir);
  return dir;
}

function fakeCompound(dir: string): string {
  const bin = join(dir, "compound");
  writeFileSync(bin, `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2" == "home backup" ]]; then
  target="$4"
  printf 'archive' > "$target"
  exit 0
fi
if [[ "$1 $2" == "home verify" ]]; then
  printf -- '- files: 1\\n'
  exit 0
fi
printf 'unexpected compound args: %s\\n' "$*" >&2
exit 2
`, { mode: 0o755 });
  return bin;
}

function fakeRsync(dir: string): string {
  const bin = join(dir, "rsync");
  writeFileSync(bin, `#!/usr/bin/env bash
set -euo pipefail
exit 0
`, { mode: 0o755 });
  return dir;
}

describe("compound_home_backup wrapper retention", () => {
  test("KEEP_LOCAL=0 without remote refuses destructive prune after verified local copy", () => {
    const dir = tempDir();
    const local = join(dir, "backups");
    const result = spawnSync("bun", [script], {
      encoding: "utf8",
      env: {
        ...process.env,
        COMPOUND_BIN: fakeCompound(dir),
        COMPOUND_HOME: join(dir, "home"),
        BACKUP_LOCAL_DIR: local,
        KEEP_LOCAL: "0",
      },
    });
    expect(result.status).toBe(1);
    expect(result.stderr).toContain("KEEP_LOCAL=0 requires BACKUP_REMOTE");
    expect(readdirSync(local).filter((name) => name.endsWith(".tar.gz")).length).toBe(1);
  });

  test("remote upload permits KEEP_LOCAL=0 prune and ignores temp names", () => {
    const dir = tempDir();
    const local = join(dir, "backups");
    writeFileSync(join(dir, "old.tmp"), "placeholder");
    const pathDir = fakeRsync(dir);
    const result = spawnSync("bun", [script], {
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${pathDir}:${process.env.PATH ?? ""}`,
        COMPOUND_BIN: fakeCompound(dir),
        COMPOUND_HOME: join(dir, "home"),
        BACKUP_LOCAL_DIR: local,
        BACKUP_REMOTE: "remote:/backups",
        KEEP_LOCAL: "0",
      },
    });
    expect(result.status).toBe(0);
    expect(result.stdout).toContain("pruned=1");
    expect(readdirSync(local).filter((name) => name.endsWith(".tar.gz")).length).toBe(0);
  });

  test("temp archive names are ignored by pruning", () => {
    const dir = tempDir();
    const local = join(dir, "backups");
    const pathDir = fakeRsync(dir);
    spawnSync("mkdir", ["-p", local]);
    const tmpArchive = join(local, "home-old.tar.gz.tmp");
    writeFileSync(tmpArchive, "temp");
    const result = spawnSync("bun", [script], {
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${pathDir}:${process.env.PATH ?? ""}`,
        COMPOUND_BIN: fakeCompound(dir),
        COMPOUND_HOME: join(dir, "home"),
        BACKUP_LOCAL_DIR: local,
        BACKUP_REMOTE: "remote:/backups",
        KEEP_LOCAL: "0",
      },
    });
    expect(result.status).toBe(0);
    expect(existsSync(tmpArchive)).toBe(true);
  });
});
