import XCTest
@testable import ClearlineKit

final class JSONChildTests: XCTestCase {
    func testImmediateExitDeliversEveryEventBeforeExit() throws {
        let done = expectation(description: "all short-lived children exited")
        done.expectedFulfillmentCount = 30
        var children: [JSONChild] = []
        for _ in 0..<30 {
            let child = JSONChild(executable: URL(fileURLWithPath: "/bin/sh"), arguments: ["-c", "printf '%s\\n' '{\"event\":\"progress\"}' '{\"event\":\"error\",\"retryable\":false}'; exit 7"])
            var events: [String] = []
            child.onEvent = { event in
                XCTAssertTrue(Thread.isMainThread)
                events.append(event["event"] as? String ?? "")
            }
            child.onExit = { code in
                XCTAssertTrue(Thread.isMainThread)
                XCTAssertEqual(events, ["progress", "error"])
                XCTAssertEqual(code, 7)
                done.fulfill()
            }
            children.append(child); try child.start()
        }
        wait(for: [done], timeout: 8)
        withExtendedLifetime(children) {}
    }
    func testPartialAndUnterminatedJSONSurvivesStderrAndEOF() throws {
        let done = expectation(description: "stdout fully drained")
        let child = JSONChild(executable: URL(fileURLWithPath: "/bin/sh"), arguments: ["-c", "printf '{\"event\":'; printf '\"last\"}'; printf 'private-upstream-log' >&2"])
        var events: [String] = []
        child.onEvent = { events.append($0["event"] as? String ?? "") }
        child.onExit = { code in XCTAssertEqual(events, ["last"]); XCTAssertEqual(code, 0); done.fulfill() }
        try child.start()
        wait(for: [done], timeout: 4)
        withExtendedLifetime(child) {}
    }
    func testClosingInputIsIdempotentAndStopsFurtherWrites() throws {
        let child = JSONChild(executable: URL(fileURLWithPath: "/usr/bin/true"), arguments: [])
        child.closeInput(); child.closeInput()
        XCTAssertThrowsError(try child.send(["action": "run"]))
    }
}
