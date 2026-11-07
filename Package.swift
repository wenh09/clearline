// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Clearline",
    platforms: [.macOS(.v13)],
    products: [.executable(name: "Clearline", targets: ["Clearline"]), .executable(name: "ClearlineNetworkAgent", targets: ["ClearlineNetworkAgent"]), .executable(name: "ClearlineTunnelAgent", targets: ["ClearlineTunnelAgent"])],
    targets: [
        .target(name: "ClearlineKit", linkerSettings: [.linkedFramework("Security"), .linkedFramework("SystemConfiguration")]),
        .executableTarget(name: "Clearline", dependencies: ["ClearlineKit"], linkerSettings: [.linkedFramework("SwiftUI"), .linkedFramework("AppKit"), .linkedFramework("Network")]),
        .executableTarget(name: "ClearlineNetworkAgent", dependencies: ["ClearlineKit"]),
        .target(name: "TunnelIPC"),
        .executableTarget(name: "ClearlineTunnelAgent", dependencies: ["ClearlineKit", "TunnelIPC"], linkerSettings: [.linkedFramework("Security")]),
        .testTarget(name: "ClearlineKitTests", dependencies: ["ClearlineKit"]),
        .testTarget(name: "ClearlineAppTests", dependencies: ["Clearline"])
    ], swiftLanguageVersions: [.v5]
)
