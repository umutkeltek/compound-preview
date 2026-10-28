package main

import (
	"fmt"

	actuatorcli "compound/internal/actuator"
	archcli "compound/internal/arch"
	browsercli "compound/internal/browser"
	executorlenscli "compound/internal/executorlens"
	gamecli "compound/internal/game/cli"
	lanematrixcli "compound/internal/lanematrix"
	noticecli "compound/internal/operatornotice"
	"compound/internal/policy"
	schedulecli "compound/internal/schedule"
	terminalcli "compound/internal/terminal/cli"
	"compound/internal/tooltopology"
)

type compoundCommand struct {
	policy.FirstPartyCommandSpec
	Run func([]string)
}

func registeredCommand(admission policy.FirstPartyCommandAdmission, run func([]string)) compoundCommand {
	return compoundCommand{FirstPartyCommandSpec: policy.NewFirstPartyCommandSpec(admission), Run: run}
}

func heldCommand(run func([]string)) compoundCommand {
	return registeredCommand(policy.FirstPartyCommandHold, run)
}

func readCommand(run func([]string)) compoundCommand {
	return registeredCommand(policy.FirstPartyCommandRead, run)
}

func governedCommand(run func([]string)) compoundCommand {
	return registeredCommand(policy.FirstPartyCommandGoverned, run)
}

func (command compoundCommand) withSubcommandAdmission(subcommand string, admission policy.FirstPartyCommandAdmission) compoundCommand {
	command.FirstPartyCommandSpec = command.FirstPartyCommandSpec.WithSubcommand(subcommand, admission)
	return command
}

func (command compoundCommand) withSubcommandFlagAdmission(subcommand, flag string, admission policy.FirstPartyCommandAdmission) compoundCommand {
	command.FirstPartyCommandSpec = command.FirstPartyCommandSpec.WithSubcommandFlag(subcommand, flag, admission)
	return command
}

// cmdVersion prints the human version string, or — with --json — the structured version report
// install verification parses. The structured form exists because a version STRING is
// precomputable: comparing one is exactly the probe a stub answers correctly (see
// internal/arch/installverify.go).
func cmdVersion(args []string) {
	if !hasFlag(args, "--json") {
		fmt.Println(versionString())
		return
	}
	printJSON(versionReport(valueFlag(args, "--challenge")))
}

func retiredCommand(name string) func([]string) {
	return func([]string) {
		cmdRetiredGen1(name)
	}
}

// compoundCommandRegistry is both executable dispatch and admission metadata.
// A command is therefore impossible to expose without choosing its outer
// posture. "hold" is the safe default for commands whose internal governance
// has not yet earned automatic first-party admission.
func compoundCommandRegistry() map[string]compoundCommand {
	return map[string]compoundCommand{
		"version":   readCommand(cmdVersion),
		"--version": readCommand(cmdVersion),
		"-v":        readCommand(cmdVersion),
		"distill":   readCommand(retiredCommand("distill")),
		"scope":     readCommand(retiredCommand("scope")),
		"promote":   readCommand(retiredCommand("promote")),
		"build":     readCommand(retiredCommand("build")),
		"reconcile": readCommand(retiredCommand("reconcile")),
		"timeline":  readCommand(retiredCommand("timeline")),
		"recall":    readCommand(retiredCommand("recall")),
		"pack":      readCommand(retiredCommand("pack")),
		"ledger":    readCommand(retiredCommand("ledger")),
		"authority": governedCommand(cmdAuthority),
		"decisions": heldCommand(cmdDecisions),
		"director":  heldCommand(cmdDirector),
		"jobprobe":  heldCommand(cmdJobProbe),
		"runtime": heldCommand(cmdRuntime).
			withSubcommandAdmission("doctor", policy.FirstPartyCommandGoverned),
		"projection":  heldCommand(cmdProjection),
		"system":      heldCommand(cmdSystem),
		"product":     heldCommand(cmdProduct),
		"source":      heldCommand(cmdSource),
		"console":     heldCommand(cmdConsole),
		"event":       heldCommand(cmdEvent),
		"event-store": heldCommand(cmdEventStore),
		"writeback":   heldCommand(cmdWriteback),
		"home": heldCommand(cmdHome).
			withSubcommandAdmission("doctor", policy.FirstPartyCommandRead).
			withSubcommandAdmission("where", policy.FirstPartyCommandRead).
			withSubcommandAdmission("verify", policy.FirstPartyCommandRead).
			withSubcommandAdmission("init", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("private-custody", policy.FirstPartyCommandGoverned),
		"store": heldCommand(cmdStore).
			withSubcommandAdmission("doctor", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("status", policy.FirstPartyCommandRead).
			withSubcommandAdmission("init", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("migrate", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("sync", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("embed", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("recall", policy.FirstPartyCommandRead),
		"envelope": heldCommand(cmdEnvelope),
		"schema":   heldCommand(cmdSchema),
		"contract": heldCommand(cmdContract).
			withSubcommandAdmission("hooks-authority", policy.FirstPartyCommandRead).
			withSubcommandAdmission("compile", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("import-hooks", policy.FirstPartyCommandGoverned),
		"orient":    governedCommand(cmdOrient),
		"work":      governedCommand(cmdWork),
		"packets":   heldCommand(cmdPackets),
		"workbench": readCommand(cmdWorkbench),
		"coordination": readCommand(cmdCoordination).
			withSubcommandAdmission("measure", policy.FirstPartyCommandGoverned),
		"attention": readCommand(cmdAttention),
		"activity":  heldCommand(cmdActivity),
		"terminal": readCommand(terminalcli.Run).
			withSubcommandAdmission("doctor", policy.FirstPartyCommandRead).
			withSubcommandAdmission("wire", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("unwire", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("hooks", policy.FirstPartyCommandRead).
			withSubcommandAdmission("workspace", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("feed", policy.FirstPartyCommandGoverned),
		"narrate":   readCommand(cmdNarrate),
		"stream":    readCommand(cmdStream),
		"executors": readCommand(cmdExecutors),
		"lanes":     readCommand(lanematrixcli.Run),
		"presence": readCommand(cmdPresence).
			withSubcommandAdmission("announce", policy.FirstPartyCommandGoverned),
		"proof-check": governedCommand(cmdProofCheck),
		"doctor":      readCommand(cmdDoctor),
		"retention": heldCommand(cmdRetention).
			withSubcommandAdmission("classes", policy.FirstPartyCommandRead).
			withSubcommandAdmission("scan", policy.FirstPartyCommandRead).
			withSubcommandAdmission("plan", policy.FirstPartyCommandRead).
			withSubcommandAdmission("apply", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("empty-trash", policy.FirstPartyCommandGoverned),
		"run-attempt":        heldCommand(cmdRunAttempt).withSubcommandAdmission("list", policy.FirstPartyCommandRead),
		"experiential-smoke": heldCommand(cmdExperientialSmoke),
		"release":            heldCommand(cmdRelease),
		"reviews":            heldCommand(cmdReviews),
		"finding":            heldCommand(cmdFinding),
		"policy":             heldCommand(cmdPolicy),
		"placement":          readCommand(cmdPlacement),
		"hook": governedCommand(cmdHook).
			withSubcommandAdmission("install", policy.FirstPartyCommandInstall).
			withSubcommandFlagAdmission("install", "--check", policy.FirstPartyCommandRead),
		"harness": governedCommand(cmdHarness).
			withSubcommandAdmission("posture-plan", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("refresh-lane", "--preflight", policy.FirstPartyCommandRead),
		"transition": readCommand(cmdTransition),
		"route": governedCommand(cmdRoute).
			withSubcommandAdmission("evidence-authority", policy.FirstPartyCommandRead).
			withSubcommandAdmission("execution-profile", policy.FirstPartyCommandRead).
			withSubcommandAdmission("experiment-plan", policy.FirstPartyCommandRead).
			withSubcommandAdmission("lane-projection", policy.FirstPartyCommandRead).
			withSubcommandAdmission("model-capabilities", policy.FirstPartyCommandRead).
			withSubcommandAdmission("promotion-apply", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("promotion-apply", "--confirm", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("promotion-revert", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("promotion-revert", "--confirm", policy.FirstPartyCommandGoverned),
		"profiles": heldCommand(cmdProfiles).
			withSubcommandAdmission("check", policy.FirstPartyCommandRead).
			withSubcommandAdmission("generate", policy.FirstPartyCommandGoverned),
		"memory": heldCommand(cmdMemory).
			withSubcommandAdmission("promote-driver", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("promote-driver", "--confirm", policy.FirstPartyCommandGoverned),
		"corpus":    heldCommand(cmdCorpus),
		"synthesis": heldCommand(cmdSynthesis),
		"knowledge": heldCommand(cmdKnowledge),
		"sessions":  heldCommand(cmdSessions),
		"distiller": heldCommand(cmdDistiller),
		"capability": heldCommand(cmdCapability).
			withSubcommandFlagAdmission("import-skills", "--dry-run", policy.FirstPartyCommandRead),
		"bench":           heldCommand(cmdBench),
		"latent-tax":      heldCommand(cmdLatentTax),
		"behavioral-lens": heldCommand(cmdBehavioralLens),
		"executor-lens":   heldCommand(executorlenscli.RunCLI),
		"lens-modulate":   heldCommand(cmdLensModulate),
		"act":             heldCommand(cmdAct).withSubcommandAdmission("credential-verify", policy.FirstPartyCommandGoverned).withSubcommandAdmission("credential-password-align", policy.FirstPartyCommandGoverned).withSubcommandAdmission("runtime-config-install", policy.FirstPartyCommandGoverned),
		"actuator":        heldCommand(actuatorcli.RunCLI),
		"capture":         heldCommand(cmdCapture),
		"intent":          heldCommand(cmdIntent),
		"land-lease":      governedCommand(cmdLandLease),
		"panel":           heldCommand(cmdPanel),
		"local-models":    heldCommand(cmdLocalModels),
		"lanebeliefs": heldCommand(cmdLaneBeliefs).
			withSubcommandAdmission("fold", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("fold", "--dry-run", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("fold", "--experimental", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("promote", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("promote", "--dry-run", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("promote", "--experimental", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("cool", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("cool", "--dry-run", policy.FirstPartyCommandRead).
			withSubcommandFlagAdmission("cool", "--experimental", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("list", policy.FirstPartyCommandRead),
		"mcp-transport": heldCommand(cmdMCPTransport),
		"negatives":     heldCommand(cmdNegatives),
		"architecture": heldCommand(archcli.Run).
			withSubcommandAdmission("doctor", policy.FirstPartyCommandRead),
		"notice": heldCommand(func(args []string) { noticecli.RunWith(args, noticeReviewQueue) }).
			withSubcommandAdmission("flush", policy.FirstPartyCommandGoverned),
		"doors": governedCommand(tooltopology.RunDoors).withSubcommandAdmission("show", policy.FirstPartyCommandRead).withSubcommandAdmission("serve", policy.FirstPartyCommandRead),
		"schedule": governedCommand(schedulecli.Run).
			withSubcommandAdmission("list", policy.FirstPartyCommandRead).
			withSubcommandAdmission("show", policy.FirstPartyCommandRead).
			withSubcommandAdmission("doctor", policy.FirstPartyCommandRead),
		"browser": readCommand(browsercli.Run).
			withSubcommandAdmission("list", policy.FirstPartyCommandRead).withSubcommandAdmission("show", policy.FirstPartyCommandRead).withSubcommandAdmission("doctor", policy.FirstPartyCommandRead).
			withSubcommandAdmission("route", policy.FirstPartyCommandRead).withSubcommandAdmission("goal", policy.FirstPartyCommandGoverned).withSubcommandAdmission("settings", policy.FirstPartyCommandRead).
			withSubcommandAdmission("provider-pricing", policy.FirstPartyCommandGoverned).withSubcommandAdmission("credential-set", policy.FirstPartyCommandGoverned).withSubcommandAdmission("credential-rotate", policy.FirstPartyCommandGoverned).
			withSubcommandFlagAdmission("settings", "--enable", policy.FirstPartyCommandGoverned).withSubcommandFlagAdmission("settings", "--disable", policy.FirstPartyCommandGoverned).withSubcommandFlagAdmission("doctor", "--fix", policy.FirstPartyCommandGoverned),
		"game": readCommand(gamecli.Run).
			withSubcommandAdmission("doctor", policy.FirstPartyCommandRead).
			withSubcommandAdmission("observe", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("genres", policy.FirstPartyCommandRead).
			withSubcommandAdmission("companies", policy.FirstPartyCommandRead).
			withSubcommandAdmission("trending", policy.FirstPartyCommandRead).
			withSubcommandAdmission("scout", policy.FirstPartyCommandRead).
			withSubcommandAdmission("draft", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("unity", policy.FirstPartyCommandGoverned).
			withSubcommandAdmission("release", policy.FirstPartyCommandGoverned),
	}
}

func firstPartyCommandCatalog() policy.FirstPartyCommandCatalog {
	registry := compoundCommandRegistry()
	specs := make(map[string]policy.FirstPartyCommandSpec, len(registry))
	for name, command := range registry {
		specs[name] = command.FirstPartyCommandSpec
	}
	return policy.NewFirstPartyCommandCatalogFromSpecs(specs)
}
