package harness

// The fixed observer calls this validator with its own pre-spawn and request clocks.
// Match the native date/CWD reminder exactly; never widen the observed date window.
const ompCanonicalValidatorJS = `function ompCanonicalNativeCwd(cwd, platform, realpath) {
  let nativeCwd = cwd;
  if (platform === "darwin" && cwd.startsWith("/private/")) {
    const stripped = cwd.slice("/private".length);
    try { if (realpath(cwd) === realpath(stripped)) nativeCwd = stripped; } catch {}
  }
  return nativeCwd.replace(/\\/g, "/");
}
function ompCanonicalValidateUserMessages(messages, prompt, cwd, startMS, requestMS) {
  const reject = () => ({ok: false});
  if (typeof prompt !== "string" || typeof cwd !== "string" || !cwd) return reject();
  if (!Number.isSafeInteger(startMS) || !Number.isSafeInteger(requestMS) ||
      requestMS < startMS || requestMS - startMS > 30000) return reject();
  const dates = [new Date(startMS), new Date(requestMS)];
  if (dates.some(d => !Number.isFinite(d.getTime()))) return reject();
  const normalizedCwd = ompCanonicalNativeCwd(cwd, process.platform, require("node:fs").realpathSync);
  const reminders = dates.map(d => {
    const date = d.getFullYear()+"-"+String(d.getMonth()+1).padStart(2,"0")+"-"+String(d.getDate()).padStart(2,"0");
    return "<system-reminder>\nToday: "+date+"; current working directory: '"+normalizedCwd+"'. Do not repeat this information in your reply.\n</system-reminder>";
  });
  if (!Array.isArray(messages) || messages.length < 1 || messages.length > 2) return reject();
  const user = messages[messages.length-1];
  if (!user || Array.isArray(user) || user.role !== "user") return reject();
  if (messages.length === 2) {
    const system = messages[0];
    if (!system || Array.isArray(system) || system.role !== "system" ||
        (typeof system.content !== "string" && !Array.isArray(system.content))) return reject();
  }
  const content = user.content;
  if (typeof content === "string") return {ok: reminders.some(r => content === r+"\n\n"+prompt)};
  if (!Array.isArray(content) || content.length !== 2 ||
      !content.every(b => b && !Array.isArray(b) && Object.keys(b).length === 2 &&
                         b.type === "text" && typeof b.text === "string")) return reject();
  return {ok: reminders.includes(content[0].text) && content[1].text === prompt};
}`
