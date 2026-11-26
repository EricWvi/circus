import { run } from "./run.js";

// The worker's entry point (dist/main.js, started by the package's launcher).
process.exitCode = await run();
