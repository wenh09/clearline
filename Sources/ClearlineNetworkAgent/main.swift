import Foundation
import ClearlineKit
import Darwin

func emit(_ value: [String: Any]) {
    if let data = try? JSONSerialization.data(withJSONObject: value), let text = String(data: data, encoding: .utf8) { print(text); fflush(stdout) }
}

let support = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("Clearline", isDirectory: true)
let preferences = NetworkPreferences(backupURL: support.appendingPathComponent("network-restore.plist"))
// A serial queue owns all preference mutations, including crash and core-exit recovery.
let queue = DispatchQueue(label: "org.clearline.client.network")
let monitor = DispatchSource.makeTimerSource(queue: queue)
var corePID: Int32 = 0
var active = false
monitor.schedule(deadline: .now() + 1, repeating: 1)
monitor.setEventHandler {
    if active && corePID > 0 && kill(corePID, 0) != 0 && errno == ESRCH {
        do { try preferences.restore(); active = false; emit(["event": "restored"]) }
        catch { emit(["event": "error", "message": error.localizedDescription]) }
    }
}
monitor.resume()
while let line = readLine() {
    guard line.utf8.count < 8192, let data = line.data(using: .utf8), let message = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { continue }
    queue.sync {
        do {
            switch message["action"] as? String {
            case "monitor":
                guard let pid = message["corePID"] as? Int, pid > 0 else { return }
                // Global TUN mode does not install an ephemeral system proxy.
                corePID = Int32(pid); active = true; emit(["event": "applied"])
            case "enable":
                guard let http = message["httpPort"] as? Int, let socks = message["socksPort"] as? Int, let pid = message["corePID"] as? Int, pid > 0 else { return }
                corePID = Int32(pid)
                try preferences.enable(httpPort: http, socksPort: socks)
                active = true; emit(["event": "applied"])
            case "disable", "recover": try preferences.restore(); active = false; emit(["event": "restored"])
            default: break
            }
        } catch {
            // enable may have committed a partial change; always attempt rollback.
            try? preferences.restore(); active = false
            emit(["event": "error", "message": error.localizedDescription])
        }
    }
}
queue.sync {
    do { try preferences.restore(); emit(["event": "restored"]) }
    catch { emit(["event": "error", "message": error.localizedDescription]) }
}
monitor.cancel()
