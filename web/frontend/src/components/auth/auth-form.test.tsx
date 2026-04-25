import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuthForm } from "./auth-form";
import { ApiError } from "@/lib/api/client";

describe("AuthForm", () => {
  it("renders title + submit label", () => {
    render(<AuthForm title="로그인" submitLabel="로그인" onSubmit={vi.fn()} />);
    expect(screen.getByRole("heading", { name: "로그인" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "로그인" })).toBeInTheDocument();
  });

  it("blocks submit with invalid input and shows field errors", async () => {
    const onSubmit = vi.fn();
    render(<AuthForm title="t" submitLabel="go" onSubmit={onSubmit} />);
    const u = userEvent.setup();
    await u.type(screen.getByLabelText("이메일"), "not-an-email");
    await u.type(screen.getByLabelText("비밀번호"), "short");
    await u.click(screen.getByRole("button", { name: "go" }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(screen.getAllByRole("alert").length).toBeGreaterThan(0);
  });

  it("calls onSubmit with parsed credentials when valid", async () => {
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(<AuthForm title="t" submitLabel="go" onSubmit={onSubmit} />);
    const u = userEvent.setup();
    await u.type(screen.getByLabelText("이메일"), "alice@example.com");
    await u.type(screen.getByLabelText("비밀번호"), "longenough01");
    await u.click(screen.getByRole("button", { name: "go" }));
    expect(onSubmit).toHaveBeenCalledWith({
      email: "alice@example.com",
      password: "longenough01",
    });
  });

  it("shows ApiError message in form-level alert", async () => {
    const onSubmit = vi
      .fn()
      .mockRejectedValue(new ApiError(401, "invalid_credentials", "invalid email or password"));
    render(<AuthForm title="t" submitLabel="go" onSubmit={onSubmit} />);
    const u = userEvent.setup();
    await u.type(screen.getByLabelText("이메일"), "alice@example.com");
    await u.type(screen.getByLabelText("비밀번호"), "longenough01");
    await u.click(screen.getByRole("button", { name: "go" }));
    expect(await screen.findByText("invalid email or password")).toBeInTheDocument();
  });
});
