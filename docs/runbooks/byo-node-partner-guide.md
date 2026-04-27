# BYO Node Partner Guide (Phase 2 Beta)

> 대상: 자기 GPU 워크스테이션을 hybrid-cloud zone에 등록하는 베타 파트너
> 소요: 30–60분 (precheck 통과 가정)
> 운영자 채널: ops@qlaud.net (모든 단계에서 막히면 즉시 연락)

이 가이드는 운영자가 안내한 일정에 맞춰 진행합니다. 운영자가 토큰 + endpoint를 안전 채널로 전달한 뒤 **§3부터 진행**해주세요.

---

## §1. 노드 준비 — BIOS / OS / 드라이버

### 1.1. BIOS

- **VT-d (Intel)** 또는 **AMD-Vi (AMD)** 활성화
- **Secure Boot** 비활성화 (vfio-pci 모듈 로딩 충돌 회피)

### 1.2. OS

- Ubuntu 22.04 LTS HWE 또는 24.04 LTS 권장 (커널 ≥ 5.15)
- `/etc/default/grub`에:
  ```
  GRUB_CMDLINE_LINUX_DEFAULT="... intel_iommu=on iommu=pt"   # Intel
  GRUB_CMDLINE_LINUX_DEFAULT="... amd_iommu=on iommu=pt"     # AMD
  ```
  적용 후 `sudo update-grub && sudo reboot`

### 1.3. NVIDIA 드라이버

```bash
# Ubuntu 24.04 — proprietary 드라이버
sudo apt install nvidia-driver-580 nvidia-utils-580
sudo reboot
```

GPU 모델별 최소 드라이버:
- Pascal/Volta: 470+
- Turing/Ampere: 535+
- Ada/Hopper: 535+

확인: `nvidia-smi` 실행 → GPU 목록 표시.

### 1.4. vfio-pci 영구 적재

```bash
echo "vfio-pci" | sudo tee /etc/modules-load.d/vfio-pci.conf
sudo modprobe vfio-pci
lsmod | grep vfio_pci    # 모듈 적재 확인
```

---

## §2. 자가 진단 — precheck 스크립트

운영자에게 받은 `byo-node-precheck.sh` 를 실행:

```bash
chmod +x byo-node-precheck.sh
sudo ./byo-node-precheck.sh
```

출력 끝에 **GO** 가 표시되면 §3 진행. **NO-GO** 라면 출력 화면을 그대로 운영자에 회신하면 분기 안내해드립니다.

---

## §3. agent 패키지 설치

운영자가 별도로 안내하는 systemd 패키지 (.deb 또는 직접 빌드된 `compute-agent` 바이너리)를 설치합니다. 설치 위치 일반:

```
/usr/local/bin/compute-agent          # 바이너리
/etc/hybrid/compute-agent.env         # 환경변수 파일
/etc/systemd/system/hybrid-compute-agent.service
```

systemd 유닛 골격:

```ini
[Unit]
Description=hybrid-cloud compute-agent
After=network-online.target libvirtd.service
Wants=network-online.target libvirtd.service

[Service]
Type=simple
EnvironmentFile=/etc/hybrid/compute-agent.env
ExecStart=/usr/local/bin/compute-agent
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

---

## §4. 환경변수 설정

운영자에게 받은 두 값을 `/etc/hybrid/compute-agent.env` 에 추가:

```bash
sudo tee -a /etc/hybrid/compute-agent.env <<'EOF'
AGENT_API_ENDPOINT=qlaud.net:443
AGENT_API_TOKEN=<운영자에게 받은 토큰>
AGENT_NODE_NAME=byo-userA-rtx4090
AGENT_VERSION=0.2.5
AGENT_PROFILE=/etc/hybrid/profile.yaml

# Phase 2 mux endpoint (운영자에게 받음)
AGENT_MUX_ENDPOINT=mux.qlaud.net:8443
EOF
sudo chmod 600 /etc/hybrid/compute-agent.env
```

⚠ `AGENT_API_TOKEN`은 한 번만 노출됩니다. 분실 시 운영자에 재발급 요청.

---

## §5. 시동 + 첫 attach 확인

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now hybrid-compute-agent
sudo journalctl -u hybrid-compute-agent -f --no-hostname
```

기대 로그 (시동 후 30초 이내):

```
{"level":"info","msg":"compute-agent starting","endpoint":"qlaud.net:443","node_name":"byo-userA-rtx4090"}
{"level":"info","msg":"registered","node_id":"<UUID>","heartbeat_seconds":15}
{"level":"info","msg":"mux endpoint configured","endpoint":"mux.qlaud.net:8443"}
{"level":"info","msg":"muxclient: attached","endpoint":"mux.qlaud.net:8443","node_id":"<UUID>"}
```

`muxclient: attached` 라인이 보이면 어태치 성공. 운영자에 노드 ID + 시각을 회신.

### 트러블슈팅 — 흔한 사례

| 증상 | 원인 | 해결 |
|---|---|---|
| `connect failed: dial tcp ...` | DNS / 방화벽 | `nc -zv mux.qlaud.net 8443` 확인 |
| `tls handshake: ... certificate signed by unknown authority` | 시스템 CA 누락 | `sudo apt install ca-certificates && sudo update-ca-certificates` |
| `auth header: invalid token` | 토큰 오타 / 개행 | env 파일 끝 개행 확인, 운영자 채널에서 토큰 재복사 |
| `agent session ended` 반복 | 토큰 폐기 또는 노드 삭제 | 운영자 회신 필요 |

---

## §6. 첫 인스턴스 생성

대시보드 https://qlaud.net 로그인 → Instances → New.

- Node 선택지에 본인 노드 (`byo-userA-rtx4090`)가 보여야 정상.
- 1×GPU, default profile로 인스턴스 생성.
- 상태가 `running` 으로 전이되면 SSH 명령이 표시됩니다:
  ```
  ssh ubuntu@<8자hex>.qlaud.net
  ```
- 본인의 SSH 공개키가 등록되어 있어야 접속 가능. 없으면 Settings → SSH Keys.

---

## §7. 운영 정책

- **재연결 시 진행 중 SSH 끊김**: NAT 라우터 재시작 / agent 재시작 시 진행 중 SSH 세션은 끊깁니다. 세션 시작은 다시 가능. (재연결 정책 NPS 조사 — Phase 2.5)
- **노드 24h 무응답**: 5분 grace period 후 인스턴스가 자동 stop 됩니다. 디스크 이미지는 보존되어 노드 복귀 시 재기동 가능.
- **토큰 폐기**: 의심되는 토큰 노출이 있으면 즉시 운영자에 알리세요. 폐기 후 60초 이내 새 mux 세션 거부됩니다.

---

## §8. 도움이 필요하면

- 운영자 채널: ops@qlaud.net
- 인입 종료/일시 중단: 같은 채널로 알리시면 안전한 폐기 절차 안내드립니다.
