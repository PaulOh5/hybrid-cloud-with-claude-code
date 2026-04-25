import { test, expect } from "@playwright/test";
import { spawnSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

// Phase 8 Checkpoint: 가입 → 로그인 → SSH 키 → 인스턴스 → SSH 접속 → 파괴
//
// Drives the full user journey end-to-end. Requires a live stack — main-api,
// ssh-proxy, and compute-agent backed by libvirt. Knobs:
//   - BASE_URL          dashboard origin (default http://qlaud.net)
//   - E2E_GPU_COUNT     0/1/2/4 (default 1)
//   - E2E_TEST_SSH      "0" to skip the ssh exec step (default: enabled)
//   - E2E_PASSWORD      override default password (>=10 chars)

const PASSWORD = process.env.E2E_PASSWORD ?? "longenough01-test";
// Default 2 matches h20a's active "1x2" profile (one size-2 slot). Override
// with E2E_GPU_COUNT=1 / 4 to exercise other layouts (requires the matching
// profile being active on the target node).
const GPU_COUNT = parseInt(process.env.E2E_GPU_COUNT ?? "2", 10);
const TEST_SSH = process.env.E2E_TEST_SSH !== "0";

test.describe.configure({ mode: "serial" });

test("Phase 8 checkpoint — register → ssh key → create → ssh → destroy", async ({ page }) => {
  // Generate a one-shot SSH keypair so we can verify cloud-init injection.
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "hc-e2e-"));
  const keyPath = path.join(tmp, "id_ed25519");
  const knownHosts = path.join(tmp, "known_hosts");
  const keygen = spawnSync(
    "ssh-keygen",
    ["-t", "ed25519", "-f", keyPath, "-N", "", "-C", "e2e@hybridcloud", "-q"],
    { encoding: "utf8" },
  );
  if (keygen.status !== 0) {
    throw new Error(`ssh-keygen failed: ${keygen.stderr || keygen.stdout}`);
  }
  const pubkey = fs.readFileSync(`${keyPath}.pub`, "utf8").trim();

  const stamp = Date.now();
  const email = `e2e-${stamp}@example.com`;
  // Distinct prefix from `email` so getByText(instanceName) doesn't match
  // the e2e-… email shown in the header.
  const instanceName = `vm-${stamp}`;
  let instanceId: string | null = null;

  try {
    // 1. Register: 가입 폼 제출 → /instances 진입 ---------------------------
    await page.goto("/register");
    await page.getByLabel("이메일").fill(email);
    await page.getByLabel("비밀번호").fill(PASSWORD);
    await page.getByRole("button", { name: "회원가입" }).click();
    await page.waitForURL("**/instances", { timeout: 30_000 });
    await expect(page.getByRole("heading", { name: "인스턴스" })).toBeVisible();

    // 2. SSH 키 등록 -------------------------------------------------------
    await page.goto("/settings/ssh-keys");
    await page.getByLabel("라벨").fill("e2e-key");
    await page.getByLabel("공개키").fill(pubkey);
    await page.getByRole("button", { name: "추가" }).click();
    await expect(page.getByText("e2e-key")).toBeVisible();

    // 3. 인스턴스 생성 -----------------------------------------------------
    await page.goto("/instances/new");
    await page.getByLabel("이름").fill(instanceName);

    // GPU 수: 0/1/2/4 버튼 중 하나 — exact text 매칭으로 충돌 회피.
    await page
      .getByRole("button", { name: String(GPU_COUNT), exact: true })
      .click();
    await page.getByRole("button", { name: "생성", exact: true }).click();

    // /instances/{uuid} 으로 리다이렉트.
    await page.waitForURL(/\/instances\/[0-9a-f-]{36}/, { timeout: 30_000 });
    instanceId = page.url().split("/").pop() ?? null;
    expect(instanceId).toBeTruthy();

    // 4. running + IP 확보 대기 -----------------------------------------
    // libvirt가 도메인을 켜자마자 'running'으로 보고하므로 상태 뱃지는 빠르게
    // 바뀌지만, ssh-ticket 발급은 vm_internal_ip가 잡힐 때까지 409를 돌려준다.
    // dnsmasq 리스/cloud-init이 끝날 때까지 dd[내부 IP]가 IPv4로 채워지길 기다림.
    await expect(page.getByText("실행 중")).toBeVisible({
      timeout: 6 * 60 * 1000,
    });
    await expect(page.getByText(/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/)).toBeVisible({
      timeout: 6 * 60 * 1000,
    });

    // SSH 카드의 명령어 코드 — 화면에는 호스트명용 <code>도 있어 nth로
    // 잡으면 모호하므로 'ssh -J'로 시작하는 첫 번째 code를 명시적으로 선택.
    const sshCodeLocator = page.locator("code", { hasText: /^ssh -J/ });
    await expect(sshCodeLocator).toBeVisible();
    const sshCommand = (await sshCodeLocator.first().textContent()) ?? "";
    expect(sshCommand).toMatch(/^ssh -J \S+ ubuntu@[0-9a-f]{8}\./);

    // 5. SSH 접속 검증 -----------------------------------------------------
    // Host key checks are disabled because (a) each VM gets a fresh host
    // key on cloud-init, and (b) ProxyJump opens two ssh hops whose option
    // inheritance via -o is fiddly — wildcard-matched config in a private
    // file applies to both hops. Safe in this E2E because we're using a
    // one-shot keypair.
    //
    // Retry: vm_internal_ip appears as soon as dnsmasq sees the lease,
    // which is well before sshd has started. We poll up to ~3 min.
    if (TEST_SSH) {
      const sshArgs = sshCommand.trim().split(/\s+/).slice(1);
      const sshConfigPath = path.join(tmp, "ssh_config");
      fs.writeFileSync(
        sshConfigPath,
        [
          "Host *",
          "  StrictHostKeyChecking no",
          `  UserKnownHostsFile ${knownHosts}`,
          "  GlobalKnownHostsFile /dev/null",
          "  ConnectTimeout 15",
          "  BatchMode yes",
          "  ServerAliveInterval 10",
          "",
        ].join("\n"),
      );

      const deadline = Date.now() + 3 * 60 * 1000;
      let lastResult: ReturnType<typeof spawnSync> | null = null;
      while (Date.now() < deadline) {
        lastResult = spawnSync(
          "ssh",
          ["-F", sshConfigPath, "-i", keyPath, ...sshArgs, "echo PHASE8-OK"],
          { encoding: "utf8", timeout: 60_000 },
        );
        if (lastResult.status === 0) break;
        await new Promise((r) => setTimeout(r, 8000));
      }
      if (!lastResult || lastResult.status !== 0) {
        throw new Error(
          `ssh exec failed (exit=${lastResult?.status}):\n  cmd=${sshCommand}\n  stdout=${lastResult?.stdout}\n  stderr=${lastResult?.stderr}`,
        );
      }
      expect(lastResult.stdout).toContain("PHASE8-OK");
    }

    // 6. 인스턴스 삭제 -----------------------------------------------------
    // 상태 머신상 'running' 상태에서 Delete = 1회: backend 202, transitions
    // to stopping → agent destroys VM → state stopped (row 보존). 2회째
    // Delete가 terminal-state 경로로 들어가서 row 제거 (204). 두 번 누른다.
    page.on("dialog", (d) => d.accept());
    await page.getByRole("button", { name: "삭제" }).click();
    await expect(page.getByText("중지됨")).toBeVisible({ timeout: 90_000 });
    // router.push의 client-side nav이 환경에 따라 안정적이지 않아 (Next.js
    // 16 + RSC), 같은 detail 페이지에서 두 번째 Delete를 실행한 다음 직접
    // 리스트로 이동해 row 사라짐을 검증.
    await page.getByRole("button", { name: "삭제" }).click();
    await page.goto("/instances");
    await expect(page.getByText(instanceName)).toBeHidden({ timeout: 60_000 });

    instanceId = null; // 정상 종료 — finally 블록의 경고 출력 회피
  } finally {
    if (instanceId) {
      // 테스트 도중 실패하면 인스턴스가 남을 수 있음. admin 토큰을 알 수
      // 없으니 자동 정리는 하지 못하고 로깅만.
      console.error(
        `[E2E] Orphaned instance left running: id=${instanceId} name=${instanceName} email=${email}`,
      );
    }
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
