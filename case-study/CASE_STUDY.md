# A founder-operated test of carried context

Compound retained two reviewed lessons, served them in later sessions, and kept inspectable records of the resulting model proposals and tests. This small trial did not demonstrate an acceptance or cost advantage from the added context.

We used two existing private Swift applications. Their clean baselines passed 59 and 223 tests. Two learning tasks produced patches that passed their original suites and five assigned acceptance checks. An agent reviewed those patches, and Compound stored the evidence and lesson bodies without promoting them into ambient memory.

Six held-out tasks each received two proposals. A received a frozen task, source and existing-test packet. B received the identical packet plus the fixed reviewed lesson relevant to that application. Both sides used the same execution and verification controls, requested model (`gpt-6.1-sol`), medium effort, and twenty-minute ceiling. Agents proposed complete source files without tools; a separate runner applied and tested them. OS checks prevented access to hidden tests, other-condition files and global skills. The comparison measures the added reviewed context.

| Result | A | B |
| --- | ---: | ---: |
| Original primary proposals accepted | 5/6 | 5/6 |
| Original simulator launch gaps | 1 | 1 |
| Accepted after paired environmental retests | 6/6 | 6/6 |
| Separate stability repeats accepted | 2/2 | 2/2 |
| Primary input tokens | 153,608 | 155,501 |
| Primary output tokens | 10,738 | 10,801 |

Two original test invocations failed because the simulator refused to launch the application before running tests. Those records remain inconclusive. Before recovery, we recorded a separate protocol and retested both unchanged proposals in each affected pair on fresh owned simulators. All four retests passed the same 59 original and two assigned tests. No model answers were regenerated, and no source or test assertions changed.

All failures and setup attempts are retained. There were 21 native executions: two accepted learning runs, twelve primary proposals, four repeat proposals, one successful connectivity probe and two Git-workspace refusals before provider interaction. Two earlier isolation setup refusals stopped before native execution.

The added context used slightly more tokens. Native elapsed totals were 177.90 seconds for A and 195.94 seconds for B; these exclude setup, testing, review and lesson preparation and do not establish a general latency difference. Active human time and dollar cost were not measured. There is no demonstrated human-effort saving, ROI, production impact or customer demand. None of the treatment explanations explicitly cited the carried finding, so retrieval and inclusion are established but a causal behavior change is not.

This was an unblinded, agent-reviewed study on the founder's private applications. Proposed fixes remain experimental candidates. The private product study cannot be independently reproduced from this repository. The accompanying synthetic runtime demo is runnable without those applications or model access; it demonstrates governed work and verifiable results, not inference. Requested and command-line model identity were observed, but the backend returned no separate identity attestation. Different-model handoff, automatic memory promotion and model training remain unproven.

The next product experiment should measure a real workflow that depends on durable business context, including active human setup, correction and review time. The current result supports an inspectable developer preview, not a commercial-performance claim.

See `experiment-results.json` for per-trial results, missing measurements and evidence hashes.
