// compound-activity-sensor — Tier-0 ambient-activity collector (slice 1).
//
// Emits a content-free NDJSON stream (compound.activity_observation.v0.2) of app focus, idle, and
// lock transitions to a local spool dir. The compound membrane (`compound activity drain`) is the
// sole consumer; this process holds ZERO authority: no titles, no URLs, no network, no
// COMPOUND_HOME access, and only zero-permission APIs (no Accessibility/TCC prompts):
//   - NSWorkspace.didActivateApplicationNotification  (app focus)
//   - CGEventSource.secondsSinceLastEventType          (idle, polled on a 30 s timer)
//   - DistributedNotificationCenter screenIsLocked/Unlocked (lock)
//
// Coverage honesty: a monotonic seq persists to a state file and a running marker detects unclean
// shutdown — the next start emits capture_gap so a silent outage never reads as inactivity.

import AppKit
import CoreGraphics
import Foundation

let schemaVersion = "compound.activity_observation.v0.2"
let engineID = "macos-activity-sensor"
let engineRevision = "0.2.0"
let idlePollSeconds: TimeInterval = 30
let idleThresholdSeconds: Double = 120

struct Engine: Codable {
    let id: String
    let revision: String
}

// The closed Tier-0 event shape. There is no title/url/document field BY SCHEMA; the membrane's
// strict decoder refuses any unknown key, so this struct must stay in lockstep with the Go side.
struct ActivityEvent: Codable {
    let schema_version: String
    let seq: Int64
    let ts: String
    let kind: String
    let bundle_id: String
    let app_name: String
    let engine: Engine
    var meta: [String: String]?
}

let fm = FileManager.default
let baseDir = fm.homeDirectoryForCurrentUser
    .appendingPathComponent("Library/Application Support/compound-activity", isDirectory: true)
let spoolDir = baseDir.appendingPathComponent("spool", isDirectory: true)
let stateURL = baseDir.appendingPathComponent("state.json")
let markerURL = baseDir.appendingPathComponent("running.marker")

let isoFormatter: ISO8601DateFormatter = {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime]
    f.timeZone = TimeZone(identifier: "UTC")
    return f
}()

let dayFormatter: DateFormatter = {
    let f = DateFormatter()
    f.dateFormat = "yyyy-MM-dd"
    f.timeZone = TimeZone(identifier: "UTC")
    f.locale = Locale(identifier: "en_US_POSIX")
    return f
}()

// Emitter serializes every write: seq is monotonic across restarts (persisted per event), spool
// files rotate by UTC day, and file modes stay 0600 (operator-private evidence).
final class Emitter {
    private let queue = DispatchQueue(label: "compound.activity.emitter")
    private var seq: Int64
    private let encoder: JSONEncoder

    // seqStateLost is true when a state file existed but could not be read — the monotonic
    // guarantee broke, which is itself a capture gap the caller must report.
    let seqStateLost: Bool

    init() {
        encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        var lost = false
        var start: Int64 = 0
        if let data = try? Data(contentsOf: stateURL) {
            if let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Int64],
                let last = obj["last_seq"] {
                start = last
            } else {
                lost = true
            }
        }
        seq = start
        seqStateLost = lost
    }

    func emit(kind: String, bundleID: String = "", appName: String = "", meta: [String: String]? = nil) {
        queue.sync {
            seq += 1
            let event = ActivityEvent(
                schema_version: schemaVersion, seq: seq, ts: isoFormatter.string(from: Date()),
                kind: kind, bundle_id: bundleID, app_name: appName,
                engine: Engine(id: engineID, revision: engineRevision), meta: meta)
            guard var line = try? encoder.encode(event) else { return }
            line.append(0x0A)
            let spoolURL = spoolDir.appendingPathComponent(dayFormatter.string(from: Date()) + ".ndjson")
            appendLine(line, to: spoolURL)
            persistSeq()
        }
    }

    private func appendLine(_ data: Data, to url: URL) {
        if !fm.fileExists(atPath: url.path) {
            fm.createFile(atPath: url.path, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        guard let handle = try? FileHandle(forWritingTo: url) else { return }
        defer { try? handle.close() }
        _ = try? handle.seekToEnd()
        try? handle.write(contentsOf: data)
    }

    private func persistSeq() {
        guard let data = try? JSONSerialization.data(withJSONObject: ["last_seq": seq]) else { return }
        try? data.write(to: stateURL, options: .atomic)
        try? fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: stateURL.path)
    }
}

// --- startup ---

try? fm.createDirectory(at: spoolDir, withIntermediateDirectories: true,
    attributes: [.posixPermissions: 0o700])

let uncleanShutdown = fm.fileExists(atPath: markerURL.path)
let emitter = Emitter()

if uncleanShutdown {
    emitter.emit(kind: "capture_gap", meta: ["reason": "unclean_shutdown"])
} else if emitter.seqStateLost {
    emitter.emit(kind: "capture_gap", meta: ["reason": "seq_state_lost"])
}
fm.createFile(atPath: markerURL.path, contents: nil, attributes: [.posixPermissions: 0o600])

// Anchor the session: a run starting mid-focus still knows the current foreground app.
if let front = NSWorkspace.shared.frontmostApplication {
    emitter.emit(kind: "app_activated",
        bundleID: front.bundleIdentifier ?? "unknown", appName: front.localizedName ?? "")
}

// --- app focus ---

NSWorkspace.shared.notificationCenter.addObserver(
    forName: NSWorkspace.didActivateApplicationNotification, object: nil, queue: .main
) { note in
    guard let app = note.userInfo?[NSWorkspace.applicationUserInfoKey] as? NSRunningApplication else {
        return
    }
    emitter.emit(kind: "app_activated",
        bundleID: app.bundleIdentifier ?? "unknown", appName: app.localizedName ?? "")
}

// --- lock / unlock ---

let distributed = DistributedNotificationCenter.default()
distributed.addObserver(
    forName: NSNotification.Name("com.apple.screenIsLocked"), object: nil, queue: .main
) { _ in emitter.emit(kind: "screen_locked") }
distributed.addObserver(
    forName: NSNotification.Name("com.apple.screenIsUnlocked"), object: nil, queue: .main
) { _ in emitter.emit(kind: "screen_unlocked") }

// --- idle (polled; min over concrete input types avoids the any-event constant, still zero-TCC) ---

let idleProbeTypes: [CGEventType] = [
    .keyDown, .mouseMoved, .leftMouseDown, .rightMouseDown, .otherMouseDown, .scrollWheel,
]

func secondsSinceLastInput() -> Double {
    idleProbeTypes.map {
        CGEventSource.secondsSinceLastEventType(.combinedSessionState, eventType: $0)
    }.min() ?? 0
}

var idle = false
let idleTimer = Timer(timeInterval: idlePollSeconds, repeats: true) { _ in
    let seconds = secondsSinceLastInput()
    if !idle && seconds >= idleThresholdSeconds {
        idle = true
        emitter.emit(kind: "idle_started", meta: ["idle_seconds": String(Int(seconds))])
    } else if idle && seconds < idleThresholdSeconds {
        idle = false
        emitter.emit(kind: "idle_ended")
    }
}
RunLoop.main.add(idleTimer, forMode: .common)

// --- clean shutdown (marker removed => no capture_gap on next start) ---

func handleShutdown(_ signalValue: Int32) -> DispatchSourceSignal {
    signal(signalValue, SIG_IGN)
    let source = DispatchSource.makeSignalSource(signal: signalValue, queue: .main)
    source.setEventHandler {
        try? fm.removeItem(at: markerURL)
        exit(0)
    }
    source.resume()
    return source
}

let termSource = handleShutdown(SIGTERM)
let intSource = handleShutdown(SIGINT)

RunLoop.main.run()
