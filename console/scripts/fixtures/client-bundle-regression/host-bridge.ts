// Server-only helper: reaches a `node:` builtin. Safe on the server, poison in
// the client bundle. Mirrors the real `@/lib/compound` leaf of regression 9c75d29.
import { execFile } from "node:child_process";

export function runHostCommand(bin: string, args: string[]): Promise<string> {
  return new Promise((resolve, reject) => {
    execFile(bin, args, (err, stdout) => (err ? reject(err) : resolve(stdout)));
  });
}
