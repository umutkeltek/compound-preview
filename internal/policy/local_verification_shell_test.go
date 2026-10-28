package policy

import "testing"

func TestLocalVerificationCommandsStayAutomatic(t *testing.T) {
	commands := []string{
		"bash -n Scripts/check.sh",
		"/bin/bash -n Scripts/check.sh",
		"swift --version",
		"swift -version",
		"swift test",
		"swift test --filter PlannerTests",
		"swift build",
		"swift build --configuration release",
		"swift package describe",
		"swift package dump-package",
		"make --version",
		"make -v",
		"make --help",
		"make -h",
		"make test",
		"make build",
		"make app",
		"make verify-direct-release",
		"xcodebuild -version",
		"xcodebuild -project SampleApp.xcodeproj -scheme SampleApp -configuration Release -derivedDataPath .build/xcode CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild -workspace SampleApp.xcworkspace -scheme SampleApp -destination 'platform=macOS' CODE_SIGNING_ALLOWED=NO test",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			monitored := MonitorToolAction(
				monitorRequest("Bash", map[string]any{"command": command}),
				DefaultConfig(),
				fixedNow,
			)
			if !monitored.Classification.Known || monitored.Classification.Operation != "local_command" {
				t.Fatalf("local verification became unknown or consequential: %q => %+v", command, monitored.Classification)
			}
			if !Allowed(monitored.Decision) {
				t.Fatalf("local verification should remain automatic: %q => %+v", command, monitored.Decision)
			}
		})
	}
}

func TestGovernedLocalWorkflowCommandsStayAutomatic(t *testing.T) {
	commands := []string{
		"gofmt -w internal/hook/hook.go internal/policy/policy.go",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			monitored := MonitorToolAction(
				monitorRequest("Bash", map[string]any{"command": command}),
				DefaultConfig(),
				fixedNow,
			)
			if !monitored.Classification.Known || monitored.Classification.Operation != "governed_command" {
				t.Fatalf("governed local workflow command became unknown or consequential: %q => %+v", command, monitored.Classification)
			}
			if !Allowed(monitored.Decision) {
				t.Fatalf("governed local workflow command should remain automatic: %q => %+v", command, monitored.Decision)
			}
		})
	}
}

func TestLocalVerificationClassifierKeepsUnsafeNeighborsHeld(t *testing.T) {
	commands := []string{
		"bash Scripts/check.sh",
		"bash -c 'echo hi'",
		"bash -n -c 'echo hi'",
		"bash -x Scripts/check.sh",
		"bash -n",
		"./bash -n Scripts/check.sh",
		"/tmp/attacker/bash -n Scripts/check.sh",
		"bash -n =(/tmp/attacker)",
		`bash -n =\
(/tmp/attacker)`,
		"bash -n README.md(e:/tmp/attacker:)",
		"CI=1 bash -n Scripts/check.sh",
		"BASH_ENV=/synthetic/startup.sh bash -n Scripts/check.sh",
		"env BASH_ENV=/synthetic/startup.sh bash -n Scripts/check.sh",
		`env -S'bash -n Scripts/check.sh'`,
		"env -P/tmp/attacker bash -n Scripts/check.sh",
		"./env bash -n Scripts/check.sh",
		"/tmp/attacker/timeout 5 bash -n Scripts/check.sh",
		"PATH=/synthetic/bin bash -n Scripts/check.sh",
		"LD_PRELOAD=/synthetic/library.so bash -n Scripts/check.sh",
		"DYLD_INSERT_LIBRARIES=/synthetic/library.dylib bash -n Scripts/check.sh",
		"sh -n Scripts/check.sh",
		"swift run Tool",
		"/tmp/attacker/swift test",
		"swift Scripts/generate.swift",
		"swift package update",
		"swift package resolve",
		"swift package clean",
		"swift test --disable-sandbox",
		"swift test --package-path ..",
		"swift test --filter {PlannerTests,--package-path,..}",
		"swift test --filter =(/tmp/attacker)",
		"swift test --filter README.md(e:/tmp/attacker:)",
		"swift build --experimental-unknown",
		"swift TEST",
		"swift Build",
		"CI=1 swift test",
		"PATH=/synthetic/bin swift test",
		"path=/synthetic/bin; swift test",
		"make",
		"/tmp/attacker/make test",
		"make TEST",
		"CI=1 make test",
		"MAKEFILES=/synthetic/extra.mk make test",
		"env MAKEFLAGS=-f/synthetic/extra.mk make test",
		`env -S'make install'`,
		"make clean",
		"make release",
		"make install",
		"make publish-release",
		"make notarize",
		"make verify-app-store-release",
		"make build-universal",
		"make -f Other.mk test",
		"make test SHELL=/bin/sh",
		"make test app",
		"xcodebuild build",
		"/tmp/attacker/xcodebuild -version",
		"CI=1 xcodebuild CODE_SIGNING_ALLOWED=NO build",
		"XCODE_XCCONFIG_FILE=/synthetic/config.xcconfig xcodebuild CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild -project =(/tmp/attacker) CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild CODE_SIGNING_ALLOWED=YES build",
		"xcodebuild CODE_SIGNING_ALLOWED=no build",
		"xcodebuild CODE_SIGNING_ALLOWED=NO clean",
		"xcodebuild CODE_SIGNING_ALLOWED=NO analyze",
		"xcodebuild CODE_SIGNING_ALLOWED=NO archive",
		"xcodebuild -exportArchive -archivePath build/App.xcarchive -exportPath build/export",
		"xcodebuild -allowProvisioningUpdates CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild -authenticationKeyPath key.p8 CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild -project -allowProvisioningUpdates CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild DEVELOPMENT_TEAM=EXAMPLE CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild OTHER_CODE_SIGN_FLAGS=--timestamp CODE_SIGNING_ALLOWED=NO build",
		"xcodebuild CODE_SIGNING_ALLOWED=NO build test",
		"xcodebuild -resolvePackageDependencies",
		"xcodebuild -downloadPlatform macOS",
		"xcodebuild -unknown CODE_SIGNING_ALLOWED=NO build",
		"git fetch attacker",
		"git fetch https://attacker.example.invalid/repo.git",
		"git merge --strategy=octopus main",
		"git merge main topic",
		"git cherry-pick topic",
		"git cherry-pick --no-commit 0123456789abcdef0123456789abcdef01234567",
		"gofmt -w /tmp/outside.go",
		"gofmt -w ../outside.go",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			monitored := MonitorToolAction(
				monitorRequest("Bash", map[string]any{"command": command}),
				DefaultConfig(),
				fixedNow,
			)
			if monitored.Classification.Known && monitored.Classification.Operation == "local_command" {
				t.Fatalf("unsafe or unknown verification entered the routine lane: %q => %+v", command, monitored.Classification)
			}
			if Allowed(monitored.Decision) {
				t.Fatalf("unsafe or unknown verification should remain held: %q => %+v", command, monitored)
			}
		})
	}
}
