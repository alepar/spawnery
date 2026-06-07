import { vi } from "vitest";

// Mock react-virtuoso so rows and Footer render in jsdom (Virtuoso virtualizes by measuring
// container height which is 0 in jsdom, so no rows/footer would render without this mock).
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({ data, itemContent, components, context }: any) => (
    <div>
      {data.map((item: any, i: number) => (
        <div key={item.id}>{itemContent(i, item)}</div>
      ))}
      {components?.Footer ? <components.Footer context={context} /> : null}
    </div>
  ),
}));

import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { MessageList } from "./MessageList";
import type { Item } from "./types";

const items: Item[] = [
  { id: 1, kind: "user", text: "sent" },
  { id: 2, kind: "user", text: "waiting", pending: true },
];

describe("MessageList", () => {
  it("shows the working footer with queued count when working", () => {
    render(<MessageList items={items} working={true} queued={2} />);
    expect(screen.getByTestId("working-indicator")).toHaveTextContent("working…");
    expect(screen.getByTestId("working-indicator")).toHaveTextContent("2 queued");
  });

  it("hides the working footer when idle", () => {
    render(<MessageList items={items} working={false} queued={0} />);
    expect(screen.queryByTestId("working-indicator")).toBeNull();
  });

  it("tags pending user bubbles as queued", () => {
    render(<MessageList items={items} working={true} queued={1} />);
    expect(screen.getByTestId("queued-tag")).toBeInTheDocument();
  });

  it("shows a turn-ended indicator when idle with a non-normal end label", () => {
    render(<MessageList items={items} working={false} endLabel="cancelled" />);
    expect(screen.getByTestId("turn-ended-indicator")).toHaveTextContent("cancelled");
  });

  it("shows no turn-ended indicator on a normal idle (null label)", () => {
    render(<MessageList items={items} working={false} endLabel={null} />);
    expect(screen.queryByTestId("turn-ended-indicator")).toBeNull();
  });

  it("prefers the working indicator over the ended one while busy", () => {
    render(<MessageList items={items} working={true} endLabel="cancelled" />);
    expect(screen.getByTestId("working-indicator")).toBeInTheDocument();
    expect(screen.queryByTestId("turn-ended-indicator")).toBeNull();
  });

  it("shows a usage badge when idle with a usage label (cat D)", () => {
    render(<MessageList items={items} working={false} usageLabel="12.3k tokens · $0.04" />);
    expect(screen.getByTestId("turn-usage-badge")).toHaveTextContent("12.3k tokens · $0.04");
  });

  it("shows no usage badge when usage is absent (graceful absence)", () => {
    render(<MessageList items={items} working={false} usageLabel={null} />);
    expect(screen.queryByTestId("turn-usage-badge")).toBeNull();
  });

  it("renders both the ended indicator and the usage badge together", () => {
    render(<MessageList items={items} working={false} endLabel="cancelled" usageLabel="500 tokens" />);
    expect(screen.getByTestId("turn-ended-indicator")).toHaveTextContent("cancelled");
    expect(screen.getByTestId("turn-usage-badge")).toHaveTextContent("500 tokens");
  });

  it("hides the usage badge while busy", () => {
    render(<MessageList items={items} working={true} usageLabel="500 tokens" />);
    expect(screen.queryByTestId("turn-usage-badge")).toBeNull();
  });
});
