import XCTest
@testable import ClearlineKit

final class ConnectionProgressTests: XCTestCase {
    func testMeasuresWholeOperationAndIndividualPhases() {
        var progress = ConnectionProgress(kind: .connect, phase: .server, now: 100)
        progress.transition(to: .checking, now: 100.4)
        progress.transition(to: .applying, now: 101.1)
        progress.finish(now: 106)
        XCTAssertEqual(progress.elapsed(at: 999), 6, accuracy: 0.001)
        XCTAssertEqual(progress.duration(of: .server, at: 999), 0.4, accuracy: 0.001)
        XCTAssertEqual(progress.duration(of: .checking, at: 999), 0.7, accuracy: 0.001)
        XCTAssertEqual(progress.duration(of: .applying, at: 999), 4.9, accuracy: 0.001)
    }
    func testRepeatedProgressDoesNotResetTimerOrFinishedMeasurement() {
        var progress = ConnectionProgress(kind: .disconnect, phase: .restoring, now: 10)
        progress.transition(to: .restoring, now: 12)
        XCTAssertEqual(progress.duration(of: .restoring, at: 13), 3, accuracy: 0.001)
        progress.finish(now: 14)
        progress.finish(now: 20)
        progress.transition(to: .server, now: 21)
        XCTAssertEqual(progress.elapsed(at: 22), 4, accuracy: 0.001)
        XCTAssertEqual(progress.phase, .restoring)
    }
    func testSwitchIncludesRestoreAndConnectionWithoutNegativeDurations() {
        var progress = ConnectionProgress(kind: .switching, phase: .restoring, now: 40)
        progress.transition(to: .server, now: 43)
        progress.transition(to: .checking, now: 44)
        XCTAssertEqual(progress.elapsed(at: 45), 5, accuracy: 0.001)
        XCTAssertEqual(progress.duration(of: .restoring, at: 45), 3, accuracy: 0.001)
        XCTAssertEqual(progress.duration(of: .checking, at: 42), 0)
    }
}
