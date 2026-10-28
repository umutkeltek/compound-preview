import { copyFile, mkdir, readFile, rename, stat } from "node:fs/promises"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

const adapterDir = dirname(fileURLToPath(import.meta.url))
const pluginSource = join(adapterDir, "compound.ts")
const registrationSource = join(adapterDir, "hook-registration.ts")
const pluginRegistration = "./plugins/compound.ts"
const pluginSDKPackage = "@opencode-ai/plugin"

type ImportAuthority = (binary: string, registration: string, home: string) => Promise<unknown>
type CompiledIntention = {
  intention: string
  disposition: string
  native_events?: string[]
  reason?: string
}
type CompiledPosture = {
  mutation_ready: boolean
  intentions: CompiledIntention[]
}
type CompilePosture = (binary: string, harness: string) => Promise<CompiledPosture>

export type InstallOptions = {
  home: string
  compoundBinary: string
  importAuthority?: ImportAuthority
  compilePosture?: CompilePosture
}

export type InstallReport = {
  config_path: string
  plugin_path: string
  backups: string[]
  plugin_registration: string
  posture_plan: CompiledPosture
  authority_import: unknown
}

async function exists(path: string): Promise<boolean> {
  try {
    await stat(path)
    return true
  } catch {
    return false
  }
}

async function backupOnce(path: string): Promise<string | undefined> {
  if (!(await exists(path))) return undefined
  const backup = path + ".bak"
  if (!(await exists(backup))) await copyFile(path, backup)
  return backup
}

async function writeAtomic(path: string, contents: string): Promise<void> {
  await mkdir(dirname(path), { recursive: true })
  const temporary = path + ".compound-install.tmp"
  await Bun.write(temporary, contents)
  await rename(temporary, path)
}

function parseConfig(raw: string, path: string): Record<string, unknown> {
  if (!raw.trim()) return {}
  const parsed = Bun.JSONC.parse(raw) as unknown
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
    throw new Error(path + " must contain a JSON object")
  }
  return parsed as Record<string, unknown>
}

export const runAuthorityImport: ImportAuthority = async (binary, registration, home) => {
  const proc = Bun.spawn(
    [binary, "contract", "import-hooks", "--hook-registration-ts", registration],
    {
      env: { ...process.env, HOME: home },
      stdout: "pipe",
      stderr: "pipe",
    },
  )
  const [exitCode, stdout, stderr] = await Promise.all([
    proc.exited,
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ])
  if (exitCode !== 0) {
    throw new Error(
      "installed-hooks import failed (" +
        exitCode +
        "): " +
        stderr.replace(/\s+/g, " ").trim(),
    )
  }
  return JSON.parse(stdout) as unknown
}

export const runPostureCompile: CompilePosture = async (binary, harness) => {
  const proc = Bun.spawn(
    [binary, "harness", "posture-plan", "--harness", harness, "--json"],
    { stdout: "pipe", stderr: "pipe" },
  )
  const [exitCode, stdout, stderr] = await Promise.all([
    proc.exited,
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ])
  if (exitCode !== 0) {
    throw new Error(
      `harness posture-plan exited ${exitCode}: ${stderr.replace(/\s+/g, " ").trim()}`,
    )
  }
  return JSON.parse(stdout) as CompiledPosture
}

function intention(plan: CompiledPosture, name: string): CompiledIntention {
  const found = plan.intentions.find((item) => item.intention === name)
  if (!found) throw new Error(`compiled posture missing ${name}`)
  return found
}

export function validateOpenCodePosture(plan: CompiledPosture): void {
  const prepare = intention(plan, "prepare_context")
  const guard = intention(plan, "guard_mutation")
  const observe = intention(plan, "observe_outcome")
  if (
    plan.mutation_ready ||
    prepare.disposition !== "native" ||
    !prepare.native_events?.includes("chat.message") ||
    guard.disposition !== "unsupported" ||
    guard.native_events?.includes("tool.execute.before") ||
    observe.disposition !== "native" ||
    !observe.native_events?.includes("tool.execute.after")
  ) {
    throw new Error("compiled OpenCode posture does not match the installed adapter contract")
  }
}

export async function installOpenCode(options: InstallOptions): Promise<InstallReport> {
  const configDir = join(options.home, ".config", "opencode")
  const configPath = join(configDir, "opencode.jsonc")
  const pluginPath = join(configDir, "plugins", "compound.ts")
  const posturePlan = await (options.compilePosture ?? runPostureCompile)(
    options.compoundBinary,
    "opencode",
  )
  validateOpenCodePosture(posturePlan)
  const backups = (
    await Promise.all([backupOnce(configPath), backupOnce(pluginPath)])
  ).filter((path): path is string => Boolean(path))

  const raw = (await exists(configPath)) ? await readFile(configPath, "utf8") : "{}"
  const config = parseConfig(raw, configPath)
  // The SDK is a package dependency for authoring plugins, not a runtime plugin factory.
  // Older host configuration registered it in both places; subtract that invalid runtime entry.
  const plugins = Array.isArray(config.plugin)
    ? config.plugin.filter((plugin) => plugin !== pluginSDKPackage)
    : []
  if (!plugins.includes(pluginRegistration)) plugins.push(pluginRegistration)
  config.plugin = plugins

  const serialized = JSON.stringify(config, null, 2) + "\n"
  JSON.parse(serialized)
  await writeAtomic(configPath, serialized)
  await mkdir(dirname(pluginPath), { recursive: true })
  await copyFile(pluginSource, pluginPath)

  const authorityImport = await (options.importAuthority ?? runAuthorityImport)(
    options.compoundBinary,
    registrationSource,
    options.home,
  )

  return {
    config_path: configPath,
    plugin_path: pluginPath,
    backups,
    plugin_registration: pluginRegistration,
    posture_plan: posturePlan,
    authority_import: authorityImport,
  }
}

function valueFlag(args: string[], name: string): string | undefined {
  const index = args.indexOf(name)
  return index >= 0 ? args[index + 1] : undefined
}

if (import.meta.main) {
  const args = Bun.argv.slice(2)
  const home = valueFlag(args, "--home") ?? process.env.HOME
  const compoundBinary =
    valueFlag(args, "--compound") ?? process.env.COMPOUND_BIN ?? Bun.which("compound")
  if (!home || !compoundBinary) {
    console.error("usage: bun adapters/opencode/install.ts [--home PATH] [--compound PATH]")
    process.exit(2)
  }
  try {
    console.log(JSON.stringify(await installOpenCode({ home, compoundBinary }), null, 2))
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exit(1)
  }
}
