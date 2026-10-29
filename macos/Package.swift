// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "OffloadMonitor",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(name: "OffloadMonitor", path: "Sources/OffloadMonitor"),
        .testTarget(name: "OffloadMonitorTests", dependencies: ["OffloadMonitor"], path: "Tests/OffloadMonitorTests"),
    ]
)
