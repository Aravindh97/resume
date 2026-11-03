import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import App from "./App";
import { setTransport } from "./lib/go";
import type { ProfileData } from "./types.gen";

afterEach(cleanup);

const profile: ProfileData = {
  name: "Aravindh Varadharaj", role: "Full-stack", focus: "Concurrency", location: "Tiruppur", email: "a@b.c",
  links: [{ label: "GitHub", url: "https://github.com/Aravindh97" }],
  projects: [
    { name: "paisa", what: "Exactly-once ledger", stack: "Go, PostgreSQL", url: "https://github.com/Aravindh97/paisa", live: "https://aravindh97.github.io/paisa/" },
    { name: "Resume", what: "This site", stack: "Go", url: "https://github.com/Aravindh97/resume" },
  ],
  summary: ["Personal projects."], experience: [{ title: "Job", dates: "2021", bullets: ["did things"] }], education: "B.E.",
};

test("shows project cards with live links and cited answers from Ask my work", async () => {
  const paths: string[] = [];
  setTransport(async (_m, path) => {
    paths.push(path);
    if (path === "api/v1/profile") return { status: 200, body: profile };
    if (path === "api/v1/evals") return { status: 200, body: [] };
    if (path === "api/v1/cluster") return { status: 200, body: { served_by: "browser", nodes: [] } };
    if (path.startsWith("api/v1/career")) return { status: 200, body: { output: "commit 1" } };
    return { status: 200, body: { query: "", low_confidence: false, took_ms: 1, served_by: "your browser", passages: [
      { repo: "paisa", file: "internal/ledger/postgres.go", start_line: 10, end_line: 20, title: "Post", text: "advisory lock", url: "https://x/y", score: 9 },
    ] } };
  });
  render(<App />);
  expect((await screen.findByRole("link", { name: "paisa live demo" })).getAttribute("href")).toBe("https://aravindh97.github.io/paisa/");
  expect(screen.queryByRole("link", { name: "Resume live demo" })).toBeNull(); // no demo for the site itself
  fireEvent.click(screen.getByText("How does paisa make payments exactly once?"));
  expect(await screen.findByText("paisa/internal/ledger/postgres.go:10-20")).toBeTruthy();
  expect(paths).toContain("api/v1/ask?q=How%20does%20paisa%20make%20payments%20exactly%20once%3F");
});
