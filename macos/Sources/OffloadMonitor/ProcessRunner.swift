import Foundation

struct ProcessOutput {
    var stdout: Data
    var stderr: Data
    var status: Int32
    var timedOut: Bool
    var truncated: Bool
    /// The caller's `ProcessControl` asked the process to stop (an interrupt was sent).
    var cancelled: Bool = false
}

/// Stops one running child cleanly: SIGINT first so the program can finish and print its result, then SIGTERM
/// after `intGrace`, then SIGKILL after `termGrace`.
///
/// The control is bound to the `Process` it signals. Binding, exit notification and every signal happen under one
/// lock, and a signal is sent only if that process is still the bound one, has not been reported as exited, and
/// `isRunning` is true. So a delayed escalation can never reach a process that already exited, and a process that
/// exited before it was bound is refused. A cancel before the start prevents the start.
final class ProcessControl: @unchecked Sendable {
    let intGrace: TimeInterval
    let termGrace: TimeInterval
    private let lock = NSLock()
    private var process: Process?
    private var exited = Set<ObjectIdentifier>()
    private var cancelledFlag = false
    private var sentSignals: [Int32] = []

    init(intGrace: TimeInterval = 30, termGrace: TimeInterval = 5) {
        self.intGrace = intGrace
        self.termGrace = termGrace
    }

    var isCancelled: Bool { lock.lock(); defer { lock.unlock() }; return cancelledFlag }
    var runningPID: Int32? { lock.lock(); defer { lock.unlock() }; return process?.processIdentifier }
    /// Signals actually sent, in order (for tests).
    var signals: [Int32] { lock.lock(); defer { lock.unlock() }; return sentSignals }

    /// Binds the control to a just-launched process. Returns false, and binds nothing, when the process already
    /// exited (its termination was reported first) or has no valid pid.
    @discardableResult
    func bind(_ p: Process) -> Bool {
        lock.lock()
        guard !exited.contains(ObjectIdentifier(p)), p.isRunning, p.processIdentifier > 1 else { lock.unlock(); return false }
        process = p
        let c = cancelledFlag
        lock.unlock()
        if c { escalate(p) }
        return true
    }

    /// Called from the termination handler. Nothing is signalled to this process afterwards, even if it was not bound yet.
    func processExited(_ p: Process) {
        lock.lock()
        exited.insert(ObjectIdentifier(p))
        if process === p { process = nil }
        lock.unlock()
    }

    func cancel() {
        lock.lock()
        if cancelledFlag { lock.unlock(); return }
        cancelledFlag = true
        let p = process
        lock.unlock()
        if let p { escalate(p) }
    }

    /// Sends `sig` only to the still-running, still-bound process, under the lock.
    private func signal(_ p: Process, _ sig: Int32) -> Bool {
        lock.lock(); defer { lock.unlock() }
        guard process === p, !exited.contains(ObjectIdentifier(p)), p.isRunning else { return false }
        let pid = p.processIdentifier
        guard pid > 1 else { return false }
        kill(pid, sig)
        sentSignals.append(sig)
        return true
    }

    private func escalate(_ p: Process) {
        guard signal(p, SIGINT) else { return }
        DispatchQueue.global().asyncAfter(deadline: .now() + intGrace) { [self] in
            guard signal(p, SIGTERM) else { return }
            DispatchQueue.global().asyncAfter(deadline: .now() + termGrace) { [self] in
                _ = signal(p, SIGKILL)
            }
        }
    }
}

/// Runs one child process without blocking any thread the caller owns.
///
/// - The process is terminated after `timeout`, escalating to SIGKILL after `killGrace`.
/// - Each stream keeps at most `maxOutput` bytes. The moment more arrives, the result is flagged
///   `truncated` and the process is terminated the same way.
/// - Optional `stdin` bytes are written on a background queue through a non-blocking descriptor with
///   SIGPIPE disabled for that descriptor only (no process-wide signal change). A child that never reads or
///   exits early cannot hang or crash the caller; the writer stops when the call finishes or the deadline passes.
/// - With a `control`, a cancel (or the timeout) interrupts the child with SIGINT and escalates on a bounded
///   schedule; `onStart` receives the child's pid right after launch.
/// - After the process exits, pipes are drained for at most `drainGrace`. A descendant that inherited
///   a pipe and keeps it open cannot hold the call open; whatever was read by then is returned.
enum ProcessRunner {
    private final class State: @unchecked Sendable {
        let lock = NSLock()
        let maxOutput: Int
        var out = Data()
        var err = Data()
        var outEOF = false
        var errEOF = false
        var exited = false
        var status: Int32 = 0
        var timedOut = false
        var truncated = false
        var finished = false
        var isFinished: Bool { lock.lock(); defer { lock.unlock() }; return finished }
        init(maxOutput: Int) { self.maxOutput = maxOutput }

        /// Appends within the cap; returns true if this chunk crossed it.
        func append(_ chunk: Data, toErr: Bool) -> Bool {
            lock.lock(); defer { lock.unlock() }
            let have = toErr ? err.count : out.count
            let room = max(0, maxOutput - have)
            if chunk.count > room { truncated = true }
            let part = chunk.prefix(room)
            if toErr { err.append(part) } else { out.append(part) }
            return chunk.count > room
        }
    }

    static func run(executable: URL, arguments: [String], environment: [String: String]? = nil,
                    timeout: TimeInterval, maxOutput: Int = 8_000_000, stdin: Data? = nil,
                    killGrace: TimeInterval = 2, drainGrace: TimeInterval = 1,
                    control: ProcessControl? = nil, onStart: (@Sendable (Int32) -> Void)? = nil) async throws -> ProcessOutput {
        if control?.isCancelled == true { throw CancellationError() }
        return try await withCheckedThrowingContinuation { (cont: CheckedContinuation<ProcessOutput, Error>) in
            let process = Process()
            process.executableURL = executable
            process.arguments = arguments
            if let environment { process.environment = environment }
            let outPipe = Pipe()
            let errPipe = Pipe()
            process.standardOutput = outPipe
            process.standardError = errPipe
            let inPipe: Pipe? = stdin == nil ? nil : Pipe()
            process.standardInput = inPipe ?? FileHandle.nullDevice

            let state = State(maxOutput: maxOutput)
            let outHandle = outPipe.fileHandleForReading
            let errHandle = errPipe.fileHandleForReading

            @Sendable func terminate() {
                if process.isRunning { process.terminate() }
                DispatchQueue.global().asyncAfter(deadline: .now() + killGrace) {
                    if process.isRunning { kill(process.processIdentifier, SIGKILL) }
                }
            }

            @Sendable func finish() {
                state.lock.lock()
                if state.finished { state.lock.unlock(); return }
                state.finished = true
                let result = ProcessOutput(stdout: state.out, stderr: state.err, status: state.status,
                                           timedOut: state.timedOut, truncated: state.truncated,
                                           cancelled: control?.isCancelled == true)
                state.lock.unlock()
                outHandle.readabilityHandler = nil
                errHandle.readabilityHandler = nil
                try? outHandle.close()
                try? errHandle.close()
                cont.resume(returning: result)
            }

            @Sendable func maybeFinish() {
                state.lock.lock()
                let ready = state.exited && state.outEOF && state.errEOF
                state.lock.unlock()
                if ready { finish() }
            }

            func watch(_ handle: FileHandle, isErr: Bool) {
                handle.readabilityHandler = { h in
                    let chunk = h.availableData
                    if chunk.isEmpty {
                        h.readabilityHandler = nil
                        state.lock.lock()
                        if isErr { state.errEOF = true } else { state.outEOF = true }
                        state.lock.unlock()
                        maybeFinish()
                        return
                    }
                    if state.append(chunk, toErr: isErr) { terminate() } // flooding output: stop at once, no graceful wait
                }
            }
            watch(outHandle, isErr: false)
            watch(errHandle, isErr: true)

            process.terminationHandler = { p in
                control?.processExited(p)
                state.lock.lock()
                state.exited = true
                state.status = p.terminationStatus
                state.lock.unlock()
                maybeFinish()
                DispatchQueue.global().asyncAfter(deadline: .now() + drainGrace) { finish() }
            }
            do {
                try process.run()
            } catch {
                process.terminationHandler = nil
                outHandle.readabilityHandler = nil
                errHandle.readabilityHandler = nil
                try? outHandle.close()
                try? errHandle.close()
                try? outPipe.fileHandleForWriting.close()
                try? errPipe.fileHandleForWriting.close()
                if let inPipe {
                    try? inPipe.fileHandleForReading.close()
                    try? inPipe.fileHandleForWriting.close()
                }
                cont.resume(throwing: error)
                return
            }
            control?.bind(process)
            onStart?(process.processIdentifier)
            // The parent's copy of the write ends must close, or EOF never arrives.
            try? outPipe.fileHandleForWriting.close()
            try? errPipe.fileHandleForWriting.close()

            if let inPipe, let stdin {
                try? inPipe.fileHandleForReading.close() // the child has its own copy
                let handle = inPipe.fileHandleForWriting
                let fd = handle.fileDescriptor
                _ = fcntl(fd, F_SETNOSIGPIPE, 1)
                _ = fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK)
                let deadline = Date().addingTimeInterval(timeout + killGrace + 1)
                DispatchQueue.global().async {
                    stdin.withUnsafeBytes { raw in
                        guard let base = raw.baseAddress else { return }
                        var offset = 0
                        while offset < raw.count, !state.isFinished, Date() < deadline {
                            let n = Darwin.write(fd, base + offset, raw.count - offset)
                            if n > 0 { offset += n }
                            else if n < 0, errno == EAGAIN || errno == EINTR { usleep(5_000) }
                            else { break } // EPIPE or another error: the child is gone or done reading
                        }
                    }
                    try? handle.close()
                }
            }

            DispatchQueue.global().asyncAfter(deadline: .now() + timeout) {
                guard process.isRunning else { return }
                state.lock.lock(); state.timedOut = true; state.lock.unlock()
                if let control { control.cancel() } else { terminate() }
            }
        }
    }
}
