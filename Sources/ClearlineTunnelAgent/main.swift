import Foundation
import Security
import Darwin
import ClearlineKit
import TunnelIPC

// No credentials enter this process. Its only client is the exact core binary
// sealed into this build. The daemon only owns a utun FD, routes and DNS.
let trustedHashes: [String] = {
    var length = 0
    guard let bytes = cl_embedded_info(&length), length > 0, length < 16384,
          let plist = try? PropertyListSerialization.propertyList(from: Data(bytes: bytes, count: length), format: nil) as? [String: Any] else { return [] }
    return plist["ClearlineCoreHashes"] as? [String] ?? []
}()
func trustedCore(_ pid: pid_t) -> Bool {
    guard !trustedHashes.isEmpty else { return false }
    var guest: SecCode?
    guard SecCodeCopyGuestWithAttributes(nil, [kSecGuestAttributePid as String: pid] as CFDictionary, [], &guest) == errSecSuccess, let guest else { return false }
    for hash in trustedHashes where hash.range(of: "^[0-9a-f]{40}$", options: .regularExpression) != nil {
        var requirement: SecRequirement?
        if SecRequirementCreateWithString("cdhash H\"\(hash)\"" as CFString, [], &requirement) == errSecSuccess,
           SecCodeCheckValidity(guest, SecCSFlags(rawValue: kSecCSStrictValidate), requirement) == errSecSuccess { return true }
    }
    return false
}
func event(_ socket: Int32, _ name: String) {
    _ = cl_write(socket, "{\"event\":\"\(name)\"}\n")
}
func message(_ socket: Int32) -> [String: Any]? {
    var buffer = [CChar](repeating: 0, count: 1024)
    let count = cl_line(socket, &buffer, buffer.count)
    guard count > 0 else { return nil }
    let data = Data(buffer.prefix(Int(count)).map { UInt8(bitPattern: $0) })
    return (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
}

if CommandLine.arguments.count == 3, CommandLine.arguments[1] == "--verify-client", let pid = Int32(CommandLine.arguments[2]) { exit(trustedCore(pid) ? 0 : 1) }
guard geteuid() == 0, !trustedHashes.isEmpty else { exit(1) }
let network = TunnelNetwork()
// launchd restarts an interrupted daemon. Restore its durable journal before
// accepting another session, including after SIGKILL or an app update.
do { try network.restore() } catch { exit(1) }
let listener = cl_listen()
guard listener >= 0 else { exit(1) }
let stateLock = NSLock()
var currentSocket: Int32 = -1
var stopping = false
signal(SIGTERM, SIG_IGN); signal(SIGINT, SIG_IGN)
var signalSources: [DispatchSourceSignal] = []
for number in [SIGTERM, SIGINT] {
    let source = DispatchSource.makeSignalSource(signal: number, queue: .global())
    source.setEventHandler {
        stateLock.lock(); stopping = true; let current = currentSocket; stateLock.unlock()
        if current >= 0 { shutdown(current, SHUT_RDWR) }
        close(listener)
    }
    source.resume(); signalSources.append(source)
}
let timer = DispatchSource.makeTimerSource(queue: .global())
timer.schedule(deadline: .now() + 2, repeating: 2)
timer.setEventHandler {
    do { try network.refresh() }
    catch {
        stateLock.lock(); let current = currentSocket; stateLock.unlock()
        if current >= 0 { shutdown(current, SHUT_RDWR) }
    }
}
timer.resume()

while true {
    stateLock.lock(); let shouldStop = stopping; stateLock.unlock()
    if shouldStop { break }
    let socket = cl_accept(listener)
    if socket < 0 { if errno == EINTR { continue }; break }
    var uid: uid_t = 0; var pid: pid_t = 0
    guard cl_peer(socket, &uid, &pid) == 0, uid != 0, trustedCore(pid) else { close(socket); continue }
    stateLock.lock(); currentSocket = socket; stateLock.unlock()
    cl_timeout(socket, 15)
    var device: Int32 = -1
    var failed = false
    do {
        guard let initial = message(socket), let action = initial["action"] as? String else { throw TunnelNetworkFailure("请求无效") }
        if action == "recover" {
            try network.restore(owner: uid); event(socket, "restored")
        } else {
            guard action == "enable", let endpoint = initial["endpoint"] as? String, TunnelNetwork.validIPv4(endpoint) else { throw TunnelNetworkFailure("入口无效") }
            try network.restore(owner: uid)
            var name = [CChar](repeating: 0, count: 32)
            device = cl_tun(&name, name.count)
            guard device >= 0, cl_send_device(socket, device) == 0,
                  message(socket)?["action"] as? String == "ready" else { throw TunnelNetworkFailure("设备未准备好") }
            try network.install(interface: String(cString: name), endpoint: endpoint, uid: uid, clientPID: pid)
            cl_timeout(socket, 0); event(socket, "applied")
            while let command = message(socket) { if command["action"] as? String == "disable" { break } }
            try network.restore(owner: uid); event(socket, "restored")
        }
    } catch {
        event(socket, "error")
        do { try network.restore(owner: uid) } catch { failed = true }
    }
    stateLock.lock(); currentSocket = -1; stateLock.unlock()
    close(socket)
    if device >= 0 { close(device) }
    if failed { exit(1) } // Let launchd retry journal recovery, never report success.
}
timer.cancel()
do { try network.restore() } catch { exit(1) }
