import Foundation

public struct ConnectionProgress: Equatable {
    public enum Kind: String { case connect, disconnect, switching, reconnect }
    public enum Phase: String, CaseIterable { case server, checking, applying, restoring, waiting }
    public let kind: Kind
    public let startedAt: TimeInterval
    public private(set) var phase: Phase
    public private(set) var phaseStartedAt: TimeInterval
    public private(set) var endedAt: TimeInterval?
    private var durations: [Phase: TimeInterval] = [:]

    public init(kind: Kind, phase: Phase, now: TimeInterval) {
        self.kind = kind; self.phase = phase; startedAt = now; phaseStartedAt = now
    }
    public mutating func transition(to next: Phase, now: TimeInterval) {
        guard endedAt == nil, next != phase else { return }
        durations[phase, default: 0] += max(0, now - phaseStartedAt)
        phase = next; phaseStartedAt = now
    }
    public mutating func finish(now: TimeInterval) {
        guard endedAt == nil else { return }
        durations[phase, default: 0] += max(0, now - phaseStartedAt)
        endedAt = max(startedAt, now)
    }
    public func elapsed(at now: TimeInterval) -> TimeInterval { max(0, (endedAt ?? now) - startedAt) }
    public func duration(of phase: Phase, at now: TimeInterval) -> TimeInterval {
        durations[phase, default: 0] + (endedAt == nil && self.phase == phase ? max(0, now - phaseStartedAt) : 0)
    }
    public func hasFinished(_ phase: Phase) -> Bool { durations[phase] != nil }
}
