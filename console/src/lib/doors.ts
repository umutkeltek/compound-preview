export type Door = {
  id: string;
  label: string;
  argv: string[];
  budgetMs: number;
  readOnly: true;
};

export const DOORS: Door[] = [
  {
    id: "attention",
    label: "Attention",
    argv: ["attention", "--json"],
    budgetMs: 1_000,
    readOnly: true,
  },
  {
    id: "executors",
    label: "Executors",
    argv: ["executors", "--json"],
    budgetMs: 500,
    readOnly: true,
  },
  {
    id: "workbench",
    label: "Workbench",
    argv: ["workbench", "--json", "--recent", "500"],
    budgetMs: 1_000,
    readOnly: true,
  },
  {
    id: "workbench-ask",
    label: "Workbench (ask drawer)",
    argv: ["workbench", "--json", "--recent", "100"],
    budgetMs: 1_000,
    readOnly: true,
  },
  {
    id: "memory-candidates",
    label: "Memory candidates",
    argv: ["memory", "candidates", "--json"],
    budgetMs: 500,
    readOnly: true,
  },
  {
    id: "operator-authority",
    label: "Operator authority",
    argv: ["policy", "operator-authority", "--json"],
    budgetMs: 500,
    readOnly: true,
  },
  {
    id: "home-doctor",
    label: "Home doctor",
    argv: ["home", "doctor", "--json"],
    budgetMs: 500,
    readOnly: true,
  },
  {
    id: "stream-snapshot",
    label: "Live stream snapshot",
    argv: ["stream", "--once", "--json"],
    budgetMs: 500,
    readOnly: true,
  },
];
