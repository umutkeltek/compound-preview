import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

import { installOpenCode } from "./install.ts"

test("installer backs up and registers the local plugin without clobbering other config", async () => {
  const home = await mkdtemp(join(tmpdir(), "compound-opencode-install-"))
  const configDir = join(home, ".config", "opencode")
  const configPath = join(configDir, "opencode.jsonc")
  await mkdir(configDir, { recursive: true })
  const original =
    "{\n" +
    "  // keep the unrelated setting\n" +
    '  "theme": "fixture",\n' +
    '  "plugin": ["existing-plugin", "@opencode-ai/plugin"]\n' +
    "}\n"
  await writeFile(configPath, original)
  const imports: string[] = []
  const posturePlans: string[] = []
  const compilePosture = async (binary: string, harness: string) => {
    posturePlans.push([binary, harness].join("|"))
    return {
      mutation_ready: false,
      intentions: [
        { intention: "prepare_context", disposition: "native", native_events: ["chat.message"] },
        {
          intention: "guard_mutation",
          disposition: "unsupported",
          native_events: [],
          reason: "tool.execute.before cannot deny execution",
        },
        { intention: "observe_outcome", disposition: "native", native_events: ["tool.execute.after"] },
      ],
    }
  }

  const first = await installOpenCode({
    home,
    compoundBinary: "/fixture/compound",
    compilePosture,
    importAuthority: async (binary, registration, importHome) => {
      imports.push([binary, registration, importHome].join("|"))
      return { hook_count: 3 }
    },
  })

  expect(await readFile(configPath + ".bak", "utf8")).toBe(original)
  expect(await readFile(first.plugin_path, "utf8")).toContain("export default CompoundPlugin")
  const config = Bun.JSONC.parse(await readFile(configPath, "utf8")) as {
    theme: string
    plugin: string[]
  }
  expect(config.theme).toBe("fixture")
  expect(config.plugin).toEqual(["existing-plugin", "./plugins/compound.ts"])
  expect(first.authority_import).toEqual({ hook_count: 3 })
  expect(posturePlans).toEqual(["/fixture/compound|opencode"])

  await installOpenCode({
    home,
    compoundBinary: "/fixture/compound",
    compilePosture,
    importAuthority: async () => ({ hook_count: 3 }),
  })
  const secondConfig = Bun.JSONC.parse(await readFile(configPath, "utf8")) as {
    plugin: string[]
  }
  expect(secondConfig.plugin).toEqual(["existing-plugin", "./plugins/compound.ts"])
  expect(imports).toHaveLength(1)
  expect(posturePlans).toHaveLength(2)
})

test("installer refuses adapter drift before writing config", async () => {
  const home = await mkdtemp(join(tmpdir(), "compound-opencode-posture-drift-"))
  const configPath = join(home, ".config", "opencode", "opencode.jsonc")

  await expect(
    installOpenCode({
      home,
      compoundBinary: "/fixture/compound",
      compilePosture: async () => ({
        mutation_ready: true,
        intentions: [
          { intention: "prepare_context", disposition: "native", native_events: ["chat.message"] },
          {
            intention: "guard_mutation",
            disposition: "native",
            native_events: ["tool.execute.before"],
          },
          { intention: "observe_outcome", disposition: "native", native_events: ["tool.execute.after"] },
        ],
      }),
      importAuthority: async () => ({ hook_count: 3 }),
    }),
  ).rejects.toThrow("does not match the installed adapter contract")
  await expect(readFile(configPath, "utf8")).rejects.toThrow()
})
