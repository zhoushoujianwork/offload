import Foundation

// The updater re-executes a copy of this binary as a detached helper. Handle that before any UI starts.
UpdateHelper.runIfRequested(arguments: CommandLine.arguments)
LocalRelaunchHelper.runIfRequested(arguments: CommandLine.arguments)
OffloadMonitorApp.main()
