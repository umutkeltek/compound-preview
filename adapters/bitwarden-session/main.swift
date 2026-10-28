import Foundation
import Security
import LocalAuthentication
import Darwin

// One bridge over the normal personal-vault CLI. Never stores the master password.
struct BridgeError: Error { let message: String }
let service = "org.compound.bitwarden-session.v1"
let account = "personal-vault"
let home = FileManager.default.homeDirectoryForCurrentUser.path
let runtimeHome = ProcessInfo.processInfo.environment["COMPOUND_HOME"] ?? home + "/.compound"
func bwExecutable() throws -> String {
    let env = ProcessInfo.processInfo.environment
    if let configured = env["COMPOUND_BITWARDEN_EXECUTABLE"], configured.hasPrefix("/"),
       FileManager.default.isExecutableFile(atPath: configured) { return configured }
    for dir in (env["PATH"] ?? "").split(separator: ":") where dir.hasPrefix("/") {
        let candidate = String(dir) + "/bw"
        if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
    }
    try fail("Bitwarden CLI is missing from PATH; install the official bw CLI first.")
}

func fail(_ message: String) throws -> Never { throw BridgeError(message: message) }
func emit(_ value: [String: Any]) {
    let data = try! JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
    FileHandle.standardOutput.write(data + Data([10]))
}
func sessionData(_ data: Data) throws -> Data {
    guard let raw = String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines),
          let decoded = Data(base64Encoded: raw), decoded.count == 64 || decoded.count == 32,
          !raw.contains("\n") else { try fail("Bitwarden returned no valid session; nothing was stored.") }
    return Data(raw.utf8)
}
func query(_ interactive: Bool = false) -> [String: Any] {
    var q: [String: Any] = [kSecClass as String: kSecClassGenericPassword,
                           kSecAttrService as String: service, kSecAttrAccount as String: account]
    if !interactive {
        let context = LAContext(); context.interactionNotAllowed = true
        q[kSecUseAuthenticationContext as String] = context
    }
    return q
}
func loadSession(interactive: Bool = false) throws -> Data? {
#if TESTING
    if let path = ProcessInfo.processInfo.environment["COMPOUND_BW_TEST_SESSION"] {
        return FileManager.default.contents(atPath: path)
    }
#endif
    var q = query(interactive); q[kSecReturnData as String] = true
    var result: CFTypeRef?
    let status = SecItemCopyMatching(q as CFDictionary, &result)
    if status == errSecItemNotFound { return nil }
    guard status == errSecSuccess, let data = result as? Data else {
        try fail("macOS Keychain is locked or denied access (\(status)); run connect in your local Terminal.")
    }
    return try sessionData(data)
}
func saveSession(_ data: Data) throws {
#if TESTING
    if let path = ProcessInfo.processInfo.environment["COMPOUND_BW_TEST_SESSION"] {
        FileManager.default.createFile(atPath: path, contents: data, attributes: [.posixPermissions: 0o600])
        return
    }
#endif
    var q = query(true)
    let status = SecItemUpdate(q as CFDictionary, [kSecValueData as String: data] as CFDictionary)
    if status == errSecItemNotFound {
        q[kSecValueData as String] = data
        q[kSecAttrLabel as String] = "Compound shared Bitwarden unlock session"
        let added = SecItemAdd(q as CFDictionary, nil)
        guard added == errSecSuccess else { try fail("Keychain did not store the session (\(added)); no plaintext fallback was used.") }
    } else if status != errSecSuccess { try fail("Keychain did not update the session (\(status)).") }
}
func removeSession() throws {
#if TESTING
    if let path = ProcessInfo.processInfo.environment["COMPOUND_BW_TEST_SESSION"] {
        if FileManager.default.fileExists(atPath: path) { try FileManager.default.removeItem(atPath: path) }
        return
    }
#endif
    let status = SecItemDelete(query(true) as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else { try fail("Keychain did not remove the session (\(status)).") }
}
func environment(_ session: Data?, interactive: Bool = false) -> [String: String] {
    var env = ProcessInfo.processInfo.environment
    env.removeValue(forKey: "BW_SESSION")
    if let session { env["BW_SESSION"] = String(data: session, encoding: .utf8) }
    env["BW_NOINTERACTION"] = interactive ? "false" : "true"
    return env
}
func foregroundTerminal(_ process: Process) throws -> () -> Void {
    guard isatty(STDIN_FILENO) != 0 else { return {} }
    let previousGroup = tcgetpgrp(STDIN_FILENO)
    guard previousGroup == getpgrp() else {
        kill(process.processIdentifier, SIGKILL); process.waitUntilExit()
        try fail("Run this command in the foreground of your local Terminal.")
    }
    // Foundation starts children in their own process group. Without transferring
    // the terminal, readline receives SIGTTIN and stops before showing a prompt.
    let previousSignal = signal(SIGTTOU, SIG_IGN)
    let childGroup = getpgid(process.processIdentifier)
    if childGroup < 0 || tcsetpgrp(STDIN_FILENO, childGroup) != 0 {
        signal(SIGTTOU, previousSignal)
        if !process.isRunning { return {} }
        kill(process.processIdentifier, SIGKILL); process.waitUntilExit()
        try fail("Could not give the child access to its Terminal; no login was completed.")
    }
    // The child may already have stopped while the parent performed the handoff.
    kill(process.processIdentifier, SIGCONT)
    return {
        _ = tcsetpgrp(STDIN_FILENO, previousGroup)
        signal(SIGTTOU, previousSignal)
    }
}
func bw(_ args: [String], session: Data? = nil, interactive: Bool = false) throws -> (Int32, Data) {
    let process = Process(); let output = Pipe()
    var path: String
#if TESTING
    if let override = ProcessInfo.processInfo.environment["COMPOUND_BW_TEST_BIN"] {
        path = override
    } else { path = try bwExecutable() }
#else
    path = try bwExecutable()
#endif
    process.executableURL = URL(fileURLWithPath: path); process.arguments = args
    process.environment = environment(session, interactive: interactive)
    process.standardOutput = output
    process.standardError = interactive ? FileHandle.standardError : FileHandle.nullDevice
    process.standardInput = interactive ? FileHandle.standardInput : FileHandle.nullDevice
    try process.run()
    let restoreTerminal = interactive ? try foregroundTerminal(process) : {}
    defer { restoreTerminal() }
    let timer = DispatchSource.makeTimerSource(queue: .global())
    timer.schedule(deadline: .now() + (interactive ? 900 : 30))
    timer.setEventHandler {
        if process.isRunning { kill(process.processIdentifier, SIGTERM) }
    }
    timer.resume(); defer { timer.cancel() }
    // Login prompts use stderr/stdin. The session is read privately from stdout.
    let data = output.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit(); return (process.terminationStatus, data)
}
func vaultStatus(_ session: Data?) throws -> String {
    let (code, data) = try bw(["status"], session: session)
    guard code == 0, let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let status = obj["status"] as? String,
          ["unauthenticated", "locked", "unlocked"].contains(status) else {
        try fail("Cannot read Bitwarden status; check the existing bw installation and network.")
    }
    return status
}

let usage = """
Shared Bitwarden personal-vault session (macOS Keychain; normal bw home).
Usage: bitwarden-session status|doctor
       bitwarden-session connect
       bitwarden-session run -- PROGRAM [ARG...]
       bitwarden-session lock

status/doctor: JSON status, no secrets. No login or Keychain prompt.
connect: local Terminal only. Reuses an unlocked session; otherwise prompts once
         through bw login/unlock and stores ONLY its session in macOS Keychain.
run: supplies BW_SESSION only to the child. Its stdout/stderr pass through.
     Use a secret-consuming program; never echo credentials into agent transcripts.
     Does not broaden permission to read, send, or mutate anything.
lock: locks the normal bw vault and removes this helper's Keychain session.
All harnesses use this command. Do not run bw unlock separately (invalidates sessions),
create per-harness vault homes, or put passwords/session keys in command arguments.
Examples:
  ~/.compound/actuator/bitwarden-session status
  ~/.compound/actuator/bitwarden-session connect
  ~/.compound/actuator/bitwarden-session run -- bw sync
  ~/.compound/actuator/bitwarden-session run -- bw list folders
"""

do {
    let args = Array(CommandLine.arguments.dropFirst())
    if args.isEmpty || args == ["--help"] || args == ["-h"] { print(usage); exit(0) }
    let command = args[0]
    guard ["status", "doctor", "connect", "run", "lock"].contains(command) else { try fail("Unknown command; use --help.") }
    guard (command == "run" && args.count >= 3 && args[1] == "--") || (command != "run" && args.count == 1) else { try fail("Invalid arguments; use --help.") }
    guard ProcessInfo.processInfo.environment["BITWARDENCLI_APPDATA_DIR"] == nil else { try fail("Unset BITWARDENCLI_APPDATA_DIR; this bridge uses the one shared bw home.") }
    var lockPath = runtimeHome + "/actuator/.bitwarden-session.lock"
#if TESTING
    lockPath = ProcessInfo.processInfo.environment["COMPOUND_BW_TEST_LOCK"] ?? lockPath
#endif
    let fd = open(lockPath, O_CREAT | O_RDWR | O_NOFOLLOW | O_CLOEXEC, S_IRUSR | S_IWUSR)
    guard fd >= 0 else { try fail("Cannot open shared session lock.") }
    defer { flock(fd, LOCK_UN); close(fd) }
    let exclusive = command == "connect" || command == "lock"
    guard flock(fd, (exclusive ? LOCK_EX : LOCK_SH) | LOCK_NB) == 0 else { try fail("Another vault operation is active; retry after it finishes.") }
    if command == "lock" {
        let (code, _) = try bw(["lock"])
        try removeSession()
        guard code == 0 else { try fail("Local shared session removed, but bw lock failed; inspect vault state.") }
        emit(["ok": true, "status": "locked"]); exit(0)
    }
    var session: Data?
    session = try loadSession(interactive: command == "connect" && isatty(STDIN_FILENO) != 0)
    if session == nil, let inherited = ProcessInfo.processInfo.environment["BW_SESSION"] {
        session = try sessionData(Data(inherited.utf8))
    }
    let status = try vaultStatus(session)
    if command == "status" || command == "doctor" {
        emit(["ok": status == "unlocked", "status": status,
              "session_source": session == nil ? "missing" : (ProcessInfo.processInfo.environment["BW_SESSION"] == nil ? "keychain" : "keychain_or_environment"),
              "next": status == "unlocked" ? "run -- PROGRAM [ARG...]" : "Run connect in your local Terminal."])
        exit(status == "unlocked" ? 0 : 3)
    }
    if command == "connect" {
        if status == "unlocked", let session { try saveSession(session); emit(["ok": true, "status": "unlocked", "reused": true]); exit(0) }
        var interactive = isatty(STDIN_FILENO) != 0 && isatty(STDERR_FILENO) != 0
#if TESTING
        interactive = ProcessInfo.processInfo.environment["COMPOUND_BW_TEST_INTERACTIVE"] == "1" || interactive
#endif
        guard interactive else { try fail("Run connect in your own Terminal; passwords must never be entered in chat or agent tool input.") }
        let (code, raw) = try bw([status == "unauthenticated" ? "login" : "unlock", "--raw"], interactive: true)
        guard code == 0 else { try fail("Bitwarden login/unlock did not complete; no session was stored.") }
        let fresh = try sessionData(raw)
        guard try vaultStatus(fresh) == "unlocked" else { try fail("Bitwarden did not verify the new session; nothing was stored.") }
        try saveSession(fresh)
        emit(["ok": true, "status": "unlocked", "session_source": "keychain"]); exit(0)
    }
    guard status == "unlocked", let session else { try fail("Vault is not connected and unlocked; run connect locally first.") }
    let child = Process(); child.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    child.arguments = Array(args.dropFirst(2)); child.environment = environment(session)
    child.standardInput = FileHandle.standardInput; child.standardOutput = FileHandle.standardOutput; child.standardError = FileHandle.standardError
    try child.run()
    let restoreTerminal = try foregroundTerminal(child)
    child.waitUntilExit(); restoreTerminal(); exit(child.terminationStatus)
} catch let error as BridgeError {
    emit(["ok": false, "error": error.message]); exit(3)
} catch {
    // Avoid rendering subprocess data, environment values or credentials in diagnostics.
    emit(["ok": false, "error": "Local session operation failed; check installed tools and Keychain access."]); exit(1)
}
