import Foundation

struct ProcessOutput {
    var stdout: Data
    var stderr: Data
    var status: Int32
    var timedOut: Bool
    var truncated: Bool
}

/// Runs one child process without blocking any thread the caller owns.
///
/// - The process is terminated after `timeout`, escalating to SIGKILL after `killGrace`.
/// - Each stream keeps at most `maxOutput` bytes. The moment more arrives, the result is flagged
///   `truncated` and the process is terminated the same way.
/// - Optional `stdin` bytes are written on a background queue through a non-blocking descriptor with
///   SIGPIPE disabled for that descriptor only (no process-wide signal change). A child that never reads or
///   exits early cannot hang or crash the caller; the writer stops when the call finishes or the deadline passes.
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
                    killGrace: TimeInterval = 2, drainGrace: TimeInterval = 1) async throws -> ProcessOutput {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<ProcessOutput, Error>) in
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
                                           timedOut: state.timedOut, truncated: state.truncated)
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
                    if state.append(chunk, toErr: isErr) { terminate() }
                }
            }
            watch(outHandle, isErr: false)
            watch(errHandle, isErr: true)

            process.terminationHandler = { p in
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
                terminate()
            }
        }
    }
}
