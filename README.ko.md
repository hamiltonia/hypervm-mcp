# hypervm-mcp

**UAC 창을 매번 클릭하지 않고, AI 코딩 에이전트로 Hyper-V를 다룹니다.**

[![CI](https://github.com/heavycaffeiner/hypervm-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/heavycaffeiner/hypervm-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

[English](README.md) | **한국어**

Hyper-V cmdlet은 하나같이 관리자 권한을 요구합니다. 에이전트에게 테스트용 VM을
하나 만들라고 하면 권한 상승 창이 뜨고, 또 뜨고, 계속 뜹니다. 결국 하루 종일
클릭하거나, 에이전트 자체를 관리자로 띄우게 되는데 후자가 더 나쁩니다.

이 프로젝트는 권한을 요청하는 대신 권한이 있는 쪽으로 작업을 옮깁니다. 작은
Windows 서비스가 Hyper-V 권한을 쥐고 있고, 에이전트는 본인 계정만 열 수 있는
명명된 파이프로 그 서비스와 이야기합니다. **설치할 때 UAC 한 번, 그 뒤로는 없습니다.**

```
Rocky Linux VM 하나 만들고 nginx 설치해서 페이지 보여줘
```

이제 이렇게 말할 수 있고, 에이전트에게는 이를 해낼 50개 남짓의 도구가 있습니다.
VM 생성, 무인 설치, VM 안에서 명령 실행, 포트 포워딩, 그리고 뭔가 잘못됐을 때
화면을 찍어 보는 것까지.

---

## 이게 뭔가요

처음이신가요? 2분치 용어만 읽고 나면 이 절은 다시 볼 일이 없습니다.

**Hyper-V**는 Windows Pro와 Windows Server에 내장된 가상 머신 엔진입니다. 내
컴퓨터 안의 상자에서 완전히 다른 운영체제(Linux, 또 다른 Windows)를 돌립니다.

**MCP**(Model Context Protocol)는 Claude Code 같은 AI 에이전트에게 호출할 도구를
쥐여 주는 표준입니다. MCP 서버가 도구 목록을 내놓으면, 언제 무엇을 어떤 인자로
호출할지는 에이전트가 판단합니다.

**hypervm-mcp**는 Hyper-V용 MCP 서버입니다. 한 번 설치해 두면, 원하는 것을
평소 말투로 이야기하고 Hyper-V 작업은 에이전트가 처리합니다.

| 이렇게 말하면 | 에이전트가 부르는 도구 |
|---|---|
| "메모리 8GB짜리 우분투 하나 만들어 줘" | `create_vm`, `create_seed_disk`, `start_vm` |
| "다 떴어?" | `wait_for_guest_ip`, `get_vm` |
| "거기에 nginx 설치해 줘" | `ssh_exec` |
| "브라우저로 열어 보고 싶어" | `open_tunnel` |
| "멈췄는데 지금 화면에 뭐 떠 있어?" | `capture_vm_screen` |
| "망가뜨리기 전에 스냅샷 하나" | `create_checkpoint` |

이 도구들을 직접 호출할 일은 없습니다. 이 문서에 이름을 적어 두는 이유는,
에이전트가 무엇을 하고 있는지 알아보고 더 구체적으로 지시할 수 있게 하기
위해서입니다.

<details>
<summary><b>마주치게 될 용어</b></summary>

| | |
|---|---|
| **호스트(host)** | Hyper-V를 돌리는 물리 Windows 컴퓨터, 즉 내 컴퓨터. |
| **게스트(guest)** | VM 안에서 돌아가는 운영체제. |
| **1세대 / 2세대** | Hyper-V의 두 가지 VM 형태. 2세대는 UEFI 기반의 현대적인 쪽이고, 1세대는 옛 운영체제를 위한 레거시 BIOS입니다. 특별한 이유가 없으면 2세대를 쓰세요. |
| **VHD / VHDX** | 가상 하드디스크. 호스트에서는 파일 하나지만 게스트는 드라이브로 봅니다. |
| **체크포인트(checkpoint)** | 되돌아갈 수 있는 VM 스냅샷. Hyper-V에서 부르는 이름입니다. |
| **가상 스위치** | 가상 랜선. *Default Switch*는 Windows에 딸려 오고 그냥 동작하며, *Internal*은 호스트와 게스트만, *Private*은 게스트끼리만 연결합니다. *External*은 게스트를 실제 LAN에 올립니다. |
| **NAT** | 주소 변환. 사설 네트워크에 있는 게스트가 인터넷에 나갈 수 있게 해 주는 장치입니다. |
| **UAC** | "이 앱이 디바이스를 변경하도록 허용하시겠어요?" 하고 묻는 Windows 창. |
| **Windows 서비스** | 부팅 때 Windows가 띄우는 백그라운드 프로그램. 내 계정보다 높은 권한으로 돌 수 있습니다. |
| **명명된 파이프(named pipe)** | 같은 컴퓨터 안의 두 프로그램을 잇는 통로. 누가 열 수 있는지는 Windows가 정합니다. |
| **VMBus** | 호스트와 게스트 사이의 Hyper-V 전용 통로. 네트워크를 쓰지 않으므로 게스트에 네트워크가 전혀 없어도 동작합니다. |
| **PowerShell Direct** | VMBus를 타고 Windows 게스트 안에서 PowerShell을 실행하는 것. 네트워크도, SSH도, 열린 포트도 필요 없습니다. |
| **통합 서비스** | 호스트가 게스트를 더 잘 보고 더 많이 시킬 수 있게 해 주는 게스트 내부의 드라이버와 데몬. Windows에는 기본 탑재이고, Linux에는 `hyperv-daemons`가 필요합니다. |
| **무인 설치 / 응답 파일** | 설치 관리자가 읽어서 사람이 클릭하지 않아도 되게 만드는 파일. Windows는 `autounattend.xml`, Rocky나 RHEL은 kickstart 파일입니다. |
| **터널(tunnel)** | 호스트의 포트를 게스트 안으로 넘겨 주는 것. `localhost:8080`이 게스트의 웹 서버에 닿습니다. |
| **Tailscale / tailnet** | 내 기기들을 잇는 사설 네트워크. 필수는 아니고, 노트북에서 VM에 접근할 때 씁니다. |

</details>

---

## 목차

- [설치](#설치)
- [첫 VM 만들기](#첫-vm-만들기)
- [VM 안의 서비스에 접근하는 세 가지 방법](#vm-안의-서비스에-접근하는-세-가지-방법)
- [발등을 찍는 것들](#발등을-찍는-것들)
- [도구 목록](#도구-목록)
- [VM에서 Windows 소프트웨어 테스트하기](#vm에서-windows-소프트웨어-테스트하기)
- [운영](#운영)
- [동작 방식](#동작-방식)
- [검증 범위](#검증-범위)

---

## 설치

시작하기 전에 필요한 것:

- **Windows 10/11 Pro, Enterprise, Education 또는 Windows Server.** Home
  에디션에는 Hyper-V가 없습니다.
- **켜져 있는 Hyper-V.** 확실하지 않다면 PowerShell에서
  `Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V`를
  실행해 보세요. `Disabled`로 나오면 *Windows 기능 켜기/끄기*에서 Hyper-V를
  켜고 재부팅합니다.
- **PowerShell 5.1.** 지원되는 Windows에는 이미 들어 있습니다.
- **MCP 클라이언트.** 예를 들어 Claude Code.

그다음 PowerShell에서:

```powershell
irm https://raw.githubusercontent.com/heavycaffeiner/hypervm-mcp/main/install.ps1 | iex
```

최신 릴리스를 내려받아 체크섬을 확인하고, 서비스를 설치하며(UAC는 여기서 한 번
뜹니다), Claude Code가 있으면 자기 자신을 등록합니다.

제대로 됐는지 확인:

```powershell
hypervm-mcp doctor
```

`doctor`는 Hyper-V, 서비스, 파이프, 저장 경로, 스위치, 자격 증명, 터널을 모두
점검하고 어긋난 것이 있으면 무엇을 고쳐야 하는지 알려 줍니다. 확인이 끝나면 MCP
클라이언트를 다시 시작해 새 서버를 인식하게 한 뒤, 에이전트에게 VM 목록을
보여 달라고 해 보세요.

나중에 업그레이드할 때:

```powershell
hypervm-mcp update
```

최신 릴리스를 확인하고 체크섬을 검증한 뒤 현재 설치 위에 덮어씁니다. 저장된 자격
증명, 고정된 호스트 키, 터널, 그리고 `config.json`에서 직접 고친 내용은 그대로
남습니다. 이것들을 지우는 것은 `service uninstall --purge`뿐입니다.

<details>
<summary>다른 설치 방법</summary>

Go로:

```powershell
go install github.com/heavycaffeiner/hypervm-mcp/cmd/hypervm-mcp@latest
hypervm-mcp service install
claude mcp add hypervm-mcp -s user -- "$env:ProgramData\hypervm-mcp\bin\hypervm-mcp.exe" bridge
```

수동으로: 릴리스에서 `hypervm-mcp.exe`를 내려받아 `hypervm-mcp.exe.sha256`과
대조한 다음 `hypervm-mcp.exe service install`을 실행합니다.

전역이 아니라 워크스페이스 단위로 쓰려면 `.mcp.json`에 이렇게 넣습니다:

```json
{
  "mcpServers": {
    "hypervm-mcp": {
      "command": "C:\\ProgramData\\hypervm-mcp\\bin\\hypervm-mcp.exe",
      "args": ["bridge"]
    }
  }
}
```
</details>

---

## 첫 VM 만들기

도구를 쓰도록 의도된 순서대로 따라가는 예제입니다.

> [!NOTE]
> **아래 상자는 직접 입력하는 것이 아닙니다.** 평소 말로 요청하면 호출은
> 에이전트가 알아서 만듭니다. 상자는 그 요청이 무엇으로 바뀌는지 보여 주는
> 것으로, 이를 알아보고 더 구체적으로 지시하라고 실어 둔 것입니다. 다만
> `hypervm-mcp`로 시작하는 블록은 예외로, PowerShell에서 직접 실행하는 실제
> 명령입니다.

**1. 만들기.** `create_vm`은 구성, 디스크, 체크포인트, 페이징 파일의 경로를 각각
따로 받습니다. 무엇도 시스템 드라이브에 떨어질 필요가 없습니다.

```
create_vm  name=dev-box  generation=2  memory_mb=4096  vhd_size_mb=32768
           iso_path=D:\ISO\Rocky-10.iso  switch_name="Default Switch"
```

**2. 지켜보지 않고 설치하기.** `create_seed_disk`는 설치 관리자가 알아서 찾아내는
작은 디스크에 응답 파일을 써 줍니다. Linux면 `OEMDRV` 레이블이 붙은 디스크의
kickstart, Windows면 ISO에 담긴 `autounattend.xml`입니다. 콘솔도 키보드도 필요
없습니다.

**3. 안으로 들어가기.** 두 가지 방법이 있고, 잘하는 일이 서로 다릅니다.

- `ssh_exec`은 게스트 OS를 가리지 않지만, 주소가 잡히고 sshd가 떠 있어야 합니다.
- `guest_invoke_command`는 Windows 게스트 전용이지만 VMBus를 타므로
  **게스트에 네트워크가 전혀 없어도** 동작합니다. 아직 닿을 수 없는 게스트를
  부트스트랩하는 방법이 이것입니다.

자격 증명은 한 번만 저장해 두면 둘 다 다시 묻지 않습니다:

```powershell
hypervm-mcp cred set --vm dev-box --user dev --ssh-key ~\.ssh\id_ed25519
```

**4. 안의 서비스에 닿기.** `open_tunnel`이 호스트 포트를 게스트 안으로 넘깁니다.
서비스가 게스트 자신의 `127.0.0.1`에 바인딩돼 있다면 `mode=ssh`를 쓰세요. 그
주소에는 다른 어떤 방법으로도 닿을 수 없습니다.

```
open_tunnel  vm_name=dev-box  guest_port=80  mode=ssh  bind_scope=loopback
```

터널은 대화가 아니라 서비스 안에 존재합니다. 대화가 끝나도 살아 있고, 서비스가
재시작되면 복원되며, 게스트가 새 주소로 재부팅해도 다시 찾아냅니다.

**5. 뭔가 잘못되면, 봅니다.** `capture_vm_screen`은 호스트 쪽에서 콘솔을 찍습니다.
에이전트도, 네트워크도, 운영체제도 필요 없습니다. 그래서 펌웨어 프롬프트,
부트 메뉴, 정지 오류 화면에서도 동작합니다. 다른 무엇도 아무것도 알려 주지 못하는
바로 그 상황에서요.

---

## VM 안의 서비스에 접근하는 세 가지 방법

VM에서 뭔가(웹 서버, 데이터베이스, 원격 데스크톱)가 돌고 있고 그것을 쓰고 싶습니다.
들어가는 길은 세 가지이고, 잘못 고르면 오후 하나를 통째로 날립니다. 그래서 각
도구가 어느 쪽에 해당하는지 알려 줍니다.

| | 언제 |
|---|---|
| **게스트 주소로 직접 연결** | 호스트 포트를 점유하지 않으므로 호스트가 이미 무엇을 듣고 있든 상관없습니다. Default Switch 위의 게스트 SMB 445가 이 방식으로 동작합니다. |
| **터널**로 호스트 포트를 안으로 넘기기 | 클라이언트를 포트로 겨눌 수 있는 것이면 무엇이든. `mode=ssh`는 게스트 자신의 루프백에 있는 서비스에 닿고, `bind_scope`가 누가 접속할 수 있는지(이 호스트만, 내 tailnet, 또는 전체)를 정합니다. |
| **External 스위치**로 게스트에게 LAN 주소 주기 | *다른 기기가* 게스트에 닿아야 하거나, 호스트가 이미 그 포트를 쥐고 있거나(SMB, RDP, WinRM), 프로토콜이 호스트 신원을 따질 때만. |

> [!CAUTION]
> **External 스위치 생성은 이 프로젝트에서 유일하게 내 컴퓨터를 망가뜨릴 수 있는
> 작업입니다.** Hyper-V가 물리 어댑터를 다시 바인딩하면서 호스트 네트워크가 몇 초
> 끊기고, 고정 주소가 옮겨 붙지 못하면 콘솔 말고는 호스트에 닿을 수 없게 됩니다.
>
> `create_switch`는 `confirm_disruption`이 설정되기 전까지 거부하며, 거부하면서
> *당신의* 호스트에 해당하는 위험을 함께 알려 줍니다. 그 어댑터가 유일한
> 업링크인지, 주소가 고정인지, 무선이라 사실상 브리지가 되지 않는지, Tailscale에는
> 무슨 일이 생기는지. `preflight_external_switch`는 같은 보고서를 보여 주되 아무것도
> 바꾸지 않습니다.
>
> **이 경로는 검증되지 않았습니다.** [검증 범위](#검증-범위)를 보세요.

---

## 발등을 찍는 것들

**여기서 VM 이름은 라벨이 아니라 신원입니다.** 자격 증명, 고정된 SSH 호스트 키,
모든 터널이 그 이름 아래 정리돼 있습니다. Hyper-V 관리자에서 VM 이름을 바꾸면 이
서버는 그 VM을 알아보지 못합니다. 자격 증명도 없고, 고정된 키도 없으며(그래서 다음
연결이 첫 대면처럼 보이고 바뀐 키가 조용히 신뢰됩니다), 터널은 다음번에 게스트를
찾을 때 실패합니다. 이 셋을 함께 옮겨 주는 `rename_vm`을 쓰세요. 다만 디스크상의
파일 이름은 바꾸지 않습니다. 폴더, 디스크, 체크포인트는 옛 이름을 그대로 유지합니다.

**게스트 IP를 알려면 게스트 안에 에이전트가 있어야 합니다.** Hyper-V가 스스로
알아내는 것이 아니라, 에이전트가 보고한 값을 읽는 것입니다. 최소 설치된 Linux에는
그것이 없어서, 멀쩡한 VM이 아무것도 보고하지 않고 `wait_for_guest_ip`가 시간
초과됩니다. `hyperv-daemons`를 설치하고 재부팅하세요. 그전까지는 `ssh_exec`,
`open_tunnel`, `diagnose_vm_network` 모두 주소를 직접 받습니다.

**Internal 스위치에는 DHCP가 없습니다.** Default Switch는 주소와 NAT와 DHCP가 이미
갖춰진 채로 오는데, 그것이 원래는 셋 다 당신 몫이라는 사실을 가려 버립니다. 직접
만든 스위치에는 아무것도 없습니다. 호스트 쪽은 `set_switch_host_address`, 게스트마다
`set_guest_static_ip`, 인터넷이 필요하면 `set_switch_nat`입니다.

**템플릿은 그 복제본이 도는 동안 꺼져 있어야 합니다.** 실행 중인 VM은 자기 디스크를
배타적으로 쥐고, 차등 디스크는 그렇게 잡힌 부모를 읽지 못합니다. 복제는 차등
디스크를 한 번도 언급하지 않는 "파일 사용 중" 오류로 실패합니다.

**중첩 가상화는 정지된 VM에서만 먹히는데, Hyper-V는 그 사실을 알려 주지 않습니다.**
실행 중인 VM에 `Set-VMProcessor -ExposeVirtualizationExtensions`를 걸면 성공했다고
보고하고는 아무것도 바꾸지 않습니다. 설정을 다시 읽어도 그대로이고, VM을 끄고 읽어도
그대로입니다. `set_vm_nested_virtualization`은 그 가짜 성공을 그대로 넘기는 대신,
Off가 아닌 VM을 거부합니다. 아울러 동적 메모리를 끕니다. Hyper-V가 요구하지는
않지만 게스트 하이퍼바이저는 요구하기 때문이고, 반환되는 상세에 바뀐 값이 나옵니다.
그리고 중첩된 게스트가 네트워크에 닿으려면 바깥 VM 어댑터가 MAC 스푸핑을 허용해야
하는데, 그것이 `set_vm_network`의 `mac_spoofing`입니다.

**경로를 여는 것은 당신이 아니라 서비스입니다.** 서비스는 자기 로그온 세션을 가진
LocalSystem으로 돌기 때문에, 당신이 연결한 네트워크 드라이브 문자는 서비스에게
존재하지 않습니다. UNC 경로를 쓰고 공유에 `HOSTNAME$` 접근을 허용하세요. 모든 경로는
무엇이 만들어지기 전에 검사됩니다.

---

## 도구 목록

대략 쉰 개입니다. 외울 필요는 없습니다. 고르는 것은 에이전트의 일입니다. 무엇이
가능한지 감을 잡을 정도로 훑어보고, 이름을 짚어 지시하고 싶을 때 펼쳐 보세요.

<details>
<summary><b>수명 주기와 체크포인트</b></summary>

| 도구 | |
|---|---|
| `list_vms` `get_vm` | 목록과 상세 정보 |
| `start_vm` `stop_vm` `restart_vm` | 전원 제어. `force`가 없으면 정상 종료 |
| `suspend_vm` `resume_vm` | 디스크에 저장하거나 메모리에서 일시 정지 |
| `wait_for_guest_ip` | 게스트에 실제로 닿을 수 있을 때까지 대기 |
| `set_vm_nested_virtualization` | 게스트가 자기 하이퍼바이저를 돌리게 하기: Hyper-V, WSL2, Docker Desktop, Windows Sandbox |
| `create_checkpoint` `list_checkpoints` | 스냅샷 생성과 조회 |
| `apply_checkpoint` | 되돌리기. 그 이후의 모든 것은 버려집니다 |
| `delete_checkpoint` | 하나 지우고 디스크 병합이 끝날 때까지 대기 |
</details>

<details>
<summary><b>VM 설정</b></summary>

`get_vm_settings`는 전체 구성을 한 번의 호출로 읽으며, 모든 설정 도구가 받아들이는
것과 같은 어휘로 돌려줍니다. 그래서 읽은 값을 그대로 되돌려 보낼 수 있습니다.

| 도구 | |
|---|---|
| `get_vm_settings` | 모든 속성 페이지의 모든 항목을 한 번에 읽기 |
| `set_vm_memory` | 시작 메모리, 동적 범위, 버퍼, 우선순위를 한꺼번에 적용 |
| `set_vm_processor` | 개수, 예약, 상한, 가중치, SMT 노출, 마이그레이션 호환성 |
| `set_vm_firmware` | 부팅 순서, 보안 부팅 템플릿, 콘솔 모드. 1세대 BIOS도 포함 |
| `set_vm_options` | 자동 시작과 중지, 체크포인트 정책, 파일 위치, 확장 세션 |
| `set_vm_integration_services` | 여기 있는 다른 도구들이 의존하는 VMBus 서비스 |
| `set_vm_security` | 가상 TPM. 이것 없이는 Windows 11이 설치되지 않습니다 |
| `set_vm_video` | `capture_vm_screen`이 찍을 콘솔 해상도 고정 |
| `set_vm_com_port` | 명명된 파이프 위의 시리얼 콘솔. 게스트 네트워크가 필요 없습니다 |
| `set_vm_disk_settings` | 정규화된 IOPS 단위의 스토리지 QoS, 그리고 컨트롤러 배치 |
</details>

<details>
<summary><b>프로비저닝과 스토리지</b></summary>

| 도구 | |
|---|---|
| `create_vm` `delete_vm` | 구성, 디스크, 체크포인트, 페이징 경로를 각각 지정 |
| `rename_vm` | VM 이름을 바꾸면서 그 아래 정리된 자격 증명, 호스트 키, 터널을 함께 이동 |
| `create_vhd` `attach_vhd` `detach_vhd` | MB 단위 크기의 디스크를 원하는 컨트롤러 포트에 |
| `add_scsi_controller` | 디스크 64개를 넘길 때 두 번째 컨트롤러 |
| `attach_iso` `eject_dvd` | 설치 미디어 넣고 빼기 |
| `create_seed_disk` | 무인 설치용 응답 파일 디스크 |
| `create_vm_from_template` | 골든 이미지에서 몇 초 만에 복제 |
| `export_vm` `import_vm` | 이미지를 만들거나, VM의 고정 구성 경로를 옮기기 |
| `get_vhd_info` `resize_vhd` `convert_vhd` | 조회하고 변형하기 |
| `get_host_storage_paths` `set_host_storage_paths` | 기본적으로 무엇이 어디에 떨어질지 |
</details>

<details>
<summary><b>네트워킹</b></summary>

| 도구 | |
|---|---|
| `list_switches` `create_switch` `delete_switch` | 가상 스위치 |
| `set_switch_host_address` `set_switch_nat` | Internal 스위치의 호스트 쪽 |
| `set_vm_network` | 스위치, 고정 MAC, VLAN, MAC 스푸핑, 어댑터 추가 |
| `set_vm_network_advanced` | DHCP와 라우터 가드, 대역폭, 오프로드, 미러링, VLAN 트렁크 |
| `remove_vm_network_adapter` | 어댑터 다시 떼어내기 |
| `set_guest_static_ip` | 게스트 안의 고정 주소 |
| `list_physical_adapters` `preflight_external_switch` | 물리 NIC를 건드리기 전에 |
| `diagnose_vm_network` | 이 VM에 무엇이 닿고 무엇이 닿지 않는지 |
</details>

<details>
<summary><b>게스트 접근</b></summary>

| 도구 | |
|---|---|
| `ssh_exec` `ssh_info` `ssh_forget_host_key` | SSH로 명령 실행. 호스트 키는 VM별로 고정 |
| `guest_invoke_command` `guest_copy_file` `guest_copy_from` | VMBus 경유. 게스트 네트워크가 전혀 없어도 |
| `guest_run_in_session` | 게스트 데스크톱에서, 권한 상승된 상태로. 창이 그려지는 곳 |
| `capture_vm_screen` | 콘솔 화면 캡처. 게스트 안에 아무것도 필요 없음 |
| `send_vm_key` `send_vm_mouse` | 콘솔 자체의 키보드와 포인터 |
| `get_guest_network` | 어댑터와 보고된 주소 |
| `open_tunnel` `list_tunnels` `close_tunnel` | 호스트 포트를 VM 안으로 |
| `tailscale_serve` `tailnet_status` | MagicDNS 이름으로 HTTPS 제공 |
| `doctor` | 전부 점검하고 무엇을 고쳐야 하는지 알려 주기 |
</details>

---

## VM에서 Windows 소프트웨어 테스트하기

<details>
<summary><b>관리자 권한이 필요한 프로그램</b></summary>

클릭할 창이 없습니다. 이 경로에는 UAC가 없기 때문입니다. `guest_invoke_command`는
VMBus를 타고 게스트에 들어가는데, 그곳의 세션은 SYSTEM으로 도는 서비스가 만들고
필터링되지 않은 관리자 토큰을 받습니다. 기능 설치, `HKLM` 아래 쓰기, 서비스 변경,
ACL 설정이 모두 그냥 됩니다.

```
guest_invoke_command  vm_name=win-test
                      command="Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0"
```

**내장 Administrator를 쓰세요.** 토큰 필터링을 절대 받지 않는 유일한 로컬
계정입니다. 다른 로컬 관리자는 필터링된 토큰으로 돌아올 수 있는데, 그 권한이
실제로 필요해지기 직전까지는 그룹 구성원처럼 보입니다. 꼭 써야 한다면 게스트에서
`LocalAccountTokenFilterPolicy`를 1로 두거나, 최고 실행 수준을 명시적으로 요구하는
`guest_run_in_session`을 거치세요.

**어려운 쪽은 권한 상승이 아니라 세션입니다.** `guest_invoke_command`는 데스크톱이
없는 세션 0에서 돕니다. 콘솔 프로그램은 신경 쓰지 않습니다. 하지만 창을 여는
프로그램은 어디에도 그려지지 않습니다.
</details>

<details>
<summary><b>GUI 검증하기</b></summary>

서로 다른 네 가지 문제입니다.

**그릴 곳.** Server Core가 아니라 데스크톱 환경, 그리고 대화형 세션이 존재하고
재부팅 후에도 유지되도록 자동 로그온. 잠금 화면, 화면 보호기, 디스플레이 절전은
끄세요. 꺼진 화면은 검은색으로 캡처되고 클릭도 받지 않는데, 이는 잠든 데스크톱이
아니라 깨진 테스트처럼 보입니다. `internal/e2e`의 응답 파일이 이 모두를 처리합니다.

**거기서 실행할 방법.** `guest_run_in_session`은 로그온한 사용자에 대해 대화형 로그온
유형의 예약 작업을 최고 실행 수준으로 등록합니다. 관리자 권한이 필요하고 *동시에*
창을 띄우는 프로그램을 다룰 수 있는 유일한 방법입니다.

**볼 방법.** `capture_vm_screen`은 호스트에서 콘솔 프레임 버퍼를 읽습니다. 이미지가
인라인으로 돌아오므로, 그림을 볼 수 있는 클라이언트라면 파일을 열 필요가 없습니다.

**조작할 방법.** `send_vm_key`와 `send_vm_mouse`가 콘솔 자체의 키보드와 포인터를
움직입니다. 마우스 좌표는 픽셀로 주되, 그 좌표를 얻은 캡처의 크기도 함께 주세요.
게스트의 실제 해상도로 맞추는 계산은 대신 해 줍니다.

> [!IMPORTANT]
> **화면은 읽는 것이지 비교하는 것이 아닙니다.** 캡처는 축소된 썸네일이고 픽셀은
> 해상도, 테마, DPI, 글꼴에 따라 움직입니다. GUI가 올바른지는
> `guest_run_in_session`에서 자동화 트리를 질의해서(`UIAutomationClient` 또는 FlaUI)
> 판단하고, 스크린샷은 무엇이 잘못됐는지 알아내는 데 쓰세요.

사람이 직접 보려면 RDP를 넘기는 편이 낫습니다. 게스트 포트 3389로 `open_tunnel`,
다른 기기에서 보려면 `bind_scope=tailnet`.
</details>

<details>
<summary><b>어떤 도구가 어떤 게스트에서 동작하는가</b></summary>

콘솔 도구들은 VM 내부의 무언가가 아니라 Hyper-V 자체의 장치를 다룹니다. 그래서
얼마나 널리 통하는지는 각 도구가 게스트에서 무엇을 필요로 하느냐에 달려 있습니다.

| | 게스트에 필요한 것 | 동작 대상 |
|---|---|---|
| `capture_vm_screen` | 전혀 없음 | Windows, Linux, OS 없는 펌웨어 |
| `send_vm_key` | 듣고 있는 무언가. 펌웨어도 해당 | Windows, Linux, OS 없는 펌웨어 |
| `send_vm_mouse` | 바인딩된 포인터. 펌웨어에는 없음 | Windows, Linux |
| `guest_invoke_command` | PowerShell Direct | Windows 전용. Linux는 `ssh_exec` |
| `guest_run_in_session` | PowerShell Direct와 데스크톱 | Windows 전용 |
| `guest_copy_file` | Guest Service Interface | 둘 다. Linux는 `hypervfcopyd` 필요 |
| `guest_copy_from` | PowerShell Direct | Windows 전용. 한 번에 파일 하나 |

`capture_vm_screen`에 `width`/`height`를 주지 않으면 콘솔 자신의 해상도를 씁니다.
언제나 받아들여지는 유일한 크기입니다. 1세대 펌웨어 화면은 640x480 언저리로,
그럴듯한 기본값과는 거리가 멉니다.

포인터는 2세대 VM에서는 합성 장치이고 1세대에서는 에뮬레이트된 PS/2 장치인데, 둘 다
처리됩니다. 키는 Hyper-V가 스캔코드로 바꾸는 Windows 가상 키 코드라서, 문자와 이동
키는 어디서나 안전한 반면 기호는 게스트의 키보드 레이아웃을 따릅니다.

Hyper-V 통합 드라이버가 없는 게스트도 캡처는 되고 펌웨어 수준에서 키도 받아들일
가능성이 높지만, 포인터는 바인딩되지 않습니다. 이 경우는 여기서 테스트되지
않았습니다.
</details>

---

## 운영

<details>
<summary><b>자격 증명과 호스트 키</b></summary>

게스트 자격 증명은 머신 범위 DPAPI로 암호화되어 서비스가 저장합니다. 그래서 대화를
거쳐 다닐 일이 없습니다:

```powershell
hypervm-mcp cred set --vm dev-box --user dev --ssh-key ~\.ssh\id_ed25519
hypervm-mcp cred list     # 무엇이 저장돼 있는지만, 비밀 자체는 절대 아님
```

CLI는 파일을 직접 쓰지 않고 파이프로 넘깁니다. 양쪽 끝이 서로 다른 계정으로 돌기
때문입니다. CLI는 당신이고, 서비스의 데이터 디렉터리에 쓸 수 있는 것은 서비스뿐입니다.

SSH 호스트 키는 첫 연결 때 VM 이름별로 고정됩니다. 이후 불일치가 생기면
`trust_new_key`를 넘기기 전까지 실패하는데, VM을 다시 만든 뒤에는 이것이 정상
경로입니다. 주소가 아니라 이름을 키로 삼은 것은 의도적입니다. 주소를 키로 삼으면
재부팅 때마다 새 호스트로 보여서 아무것도 고정하지 못합니다.
</details>

<details>
<summary><b>CLI, 파일 위치, 문제 해결</b></summary>

```
hypervm-mcp bridge                  MCP 트래픽 중계 (MCP 클라이언트가 실행)
hypervm-mcp service install         설치 후 시작 (UAC 한 번)
hypervm-mcp service uninstall       제거. --purge는 저장된 데이터까지 삭제
hypervm-mcp service start | stop | status
hypervm-mcp update                  최신 릴리스를 현재 설치 위에 덮어쓰기
hypervm-mcp cred set | list | delete
hypervm-mcp tunnel list
hypervm-mcp doctor                  설정을 점검하고 고칠 것을 보고
hypervm-mcp version
```

```
%ProgramData%\hypervm-mcp\
  bin\hypervm-mcp.exe    서비스가 실행하는 바이너리
  config.json            파이프 이름, 허용 SID, PowerShell 경로, 제한값
  credentials.dat        게스트 자격 증명, DPAPI 머신 범위
  known_hosts.json       고정된 SSH 호스트 키, VM 이름별
  tunnels.json           터널 정의. 재시작 시 다시 열림
  logs\service.log       경고와 오류는 이벤트 로그에도 기록됨
```

이 디렉터리의 ACL은 명시적이고 상속받지 않습니다. LocalSystem과 Administrators가
모든 권한을 갖고, 설치한 사용자는 읽기 전용입니다. 바이너리가 빌드 트리가 아니라
여기 사는 이유는, 관리자가 아닌 사용자가 쓸 수 있으면서 LocalSystem 서비스가
실행하는 파일은 그 자체로 권한 상승 통로이기 때문입니다.

먼저 `hypervm-mcp doctor`를 실행하세요. Hyper-V, 파이프, 저장 경로, 스위치, 자격
증명, Tailscale, 열려 있는 모든 터널을 점검하고 무엇을 하면 되는지 알려 줍니다.

| 증상 | |
|---|---|
| *"the hypervm-mcp service is not running"* | `hypervm-mcp service start` |
| *"access to \\\\.\\pipe\\hypervm-mcp was denied"* | 파이프는 설치 당시의 계정만 받아들입니다. 그 계정으로 다시 설치하세요. |
| `ACCESS_DENIED` *"Hyper-V refused the caller"* | PowerShell이 Hyper-V 권한 없이 실행됐습니다. 서비스가 LocalSystem으로 도는지 확인하세요. |
| `GUEST_IP_UNAVAILABLE` | 대개 위에서 말한 게스트 에이전트가 없는 경우입니다. |
| `SSH_HOST_KEY_MISMATCH` | VM을 다시 만든 뒤라면 정상입니다. `trust_new_key`를 넘기거나 `ssh_forget_host_key`를 쓰세요. |
| 445, 3389, 5985에서 `PORT_IN_USE` | Windows가 쥐고 있는 포트라 터널링할 수 없습니다. |
</details>

---

## 동작 방식

권한 없는 브리지가 명명된 파이프를 통해 MCP를 중계하고, 반대편의 LocalSystem
서비스가 Hyper-V 권한을 쥡니다. 파이프의 DACL은 보호돼 있고 당신의 계정만을
지정하므로, 경계를 지키는 것은 이 코드의 조심성이 아니라 Windows입니다.

<details>
<summary><b>설계 노트</b></summary>

**인자는 스크립트 본문에 절대 들어가지 않습니다.** 모든 값은 stdin으로 JSON으로
전달되어 PowerShell 스크립트 안에서 `$P`로 읽힙니다. `Dev"; Remove-Item C:\ -Recurse`
라는 이름의 VM은 코드가 아니라 문자열입니다. 바로 그것에 대한 테스트가 있습니다.

**성공은 종료 코드가 아니라 필드입니다.** PowerShell은 종료되지 않는 오류에 대해
종료 코드를 0으로 두므로, 스크립트는 `ok`를 명시적으로 보고하는 봉투에 감싸고
`$ErrorActionPreference = 'Stop'`으로 모든 오류를 종료 오류로 승격시킵니다.

**투영은 해시테이블로 만들고 `Select-Object`의 계산된 속성은 쓰지 않습니다.**
Select-Object는 표현식의 출력을 평탄화합니다. 원소가 하나인 배열은 스칼라가 되고,
빈 배열은 `{}`가 됩니다. 둘 다 잘못 디코딩되는데, 하필 원소 하나인 경우가 흔합니다.
VM에는 보통 IP 주소가 정확히 하나 있으니까요.

**호스트의 표시 언어에 의존하는 것은 없습니다.** Hyper-V는 메시지를 현지화하므로,
한국어 Windows에서는 "VM을 찾을 수 없음"이 한국어로 옵니다. 결과 판정은 로케일과
무관한 오류 *범주*와 cmdlet id로 하고, 텍스트 매칭 폴백까지 내려오는 것이 영어이도록
`en-US`를 고정해 둡니다.
</details>

<details>
<summary><b>설치된 사본과 나란히 개발하기</b></summary>

이미 hypervm-mcp를 실제 작업에 쓰고 있다면, 개발 빌드가 그 서비스를 재시작하거나
자격 증명을 읽거나 그 파이프에 응답하는 일은 원치 않을 것입니다. 인스턴스 이름을
넣어 빌드하면 처음부터 끝까지 별도의 신원을 갖습니다:

```powershell
go build -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev" `
  -o bin\hypervm-mcp-dev.exe .\cmd\hypervm-mcp
.\bin\hypervm-mcp-dev.exe service install
```

| | 릴리스 빌드 | `instance=dev` |
|---|---|---|
| 서비스 | `hypervm-mcp` | `hypervm-mcp-dev` |
| 파이프 | `\\.\pipe\hypervm-mcp` | `\\.\pipe\hypervm-mcp-dev` |
| 데이터 디렉터리 | `%ProgramData%\hypervm-mcp` | `%ProgramData%\hypervm-mcp-dev` |
| 이벤트 로그 원본 | `hypervm-mcp` | `hypervm-mcp-dev` |
| 방화벽 규칙과 NAT | `hypervm-mcp-*` | `hypervm-mcp-dev-*` |
| MCP 서버 이름 | `hypervm-mcp` | `hypervm-mcp-dev` |

릴리스 빌드는 아무 플래그도 넘기지 않으므로 이름이 그대로입니다. 자격 증명과 고정된
호스트 키는 넘어오지 않습니다. dev 인스턴스는 비어 있는 상태로 시작하고, 그것이
핵심입니다.

**Hyper-V 자체는 격리되지 않습니다.** 두 인스턴스는 같은 하이퍼바이저를 다루고 같은
가상 머신들을 봅니다. VM 이름을 서로 겹치지 않게 하는 것은 여전히 당신 몫입니다.
</details>

<details>
<summary><b>테스트</b></summary>

```powershell
go test ./...
```

실제 인프라를 건드리는 것은 전부 옵트인입니다. VM 하나가 드는 일이니까요.

```powershell
$env:HYPERVM_E2E = "$env:ProgramData\hypervm-mcp\bin\hypervm-mcp.exe"
go test ./internal/e2e -count=1 -v          # 설치된 서비스를 대상으로

$env:HYPERVM_E2E_ROCKY = "1"
$env:HYPERVM_E2E_INSTALL = "1"              # VM을 다시 만듦. 별도 게이트
go test ./internal/e2e -run Rocky -count=1 -v -timeout 90m
```

`TestRockyProvision`은 `OEMDRV` 시드 디스크의 kickstart로 Rocky Linux 10을 무인
설치합니다. Anaconda가 부팅 파라미터도, 재작성한 ISO도 없이 알아서 찾아냅니다.
나머지 테스트는 그 게스트 위에 쌓입니다. SSH 터널 뒤의 nginx, 호스트에서 붙는 SMB,
붙인 디스크들로 만든 RAID 배열, 세 노드짜리 사설 네트워크.

dev 인스턴스를 테스트하려면 테스트 바이너리에도 같은 `-ldflags`가 필요합니다.
서버와 같은 방식으로 파이프 이름을 결정하기 때문입니다.
</details>

---

## 검증 범위

여기 있는 것은 모두 구현돼 있습니다. 이 절은 그중 어디까지가 실제 하드웨어에서
돌려 본 것인지를 밝힙니다. 공개된 서버에서는 그 둘이 서로 다른 주장이기 때문입니다.

<details open>
<summary><b>끝까지 돌려 본 것</b></summary>

Windows 11 위의 **Rocky Linux 10** 대상:

| | |
|---|---|
| 무인 설치 | `OEMDRV` 시드 디스크의 kickstart로 3.5분, 콘솔 입력 없음 |
| SSH 터널 | 게스트의 `127.0.0.1`에 바인딩된 nginx. `direct` 터널이 먼저 실패하는 것까지 확인 |
| Tailnet 터널 | tailnet 주소 양쪽 모두. 방화벽 규칙 생성과 삭제를 Windows에 대조해 확인 |
| `tailscale serve` | MagicDNS 이름에서 HTTPS로 제공 |
| 호스트에서 게스트 SMB | Default Switch를 통한 읽기와 쓰기. 445 터널이 거부되는 것도 함께 |
| 체크포인트 | 스냅샷, 게스트 변경, 되돌리기, 변경이 사라진 것 확인, 병합 |
| 골든 이미지 복제 | 32GB VM을 6.9초에. 복제본을 지워도 원본 이미지는 그대로 |
| 디스크와 RAID | 지정한 포트에 512MB 고정 디스크 네 개, 게스트가 RAID5로 구성 |
| ISO 마운트 | 실행 중인 VM에 붙여서 게스트가 마운트하고 읽음 |
| 사설 네트워크 | Internal 스위치, 고정 IP, 호스트와 게스트 둘이 서로 모두 도달, NAT까지 |
| 네트워크 진단 | 포트 탐색, 그리고 "주소 보고 없음"과 "도달 불가"를 구분 |
| 콘솔 캡처 | 텍스트 콘솔과 Wayland 세션 양쪽을 호스트에서 읽음 |
| 콘솔 키보드 | Linux 텍스트 콘솔에 키가 도달하고 화면이 그에 반응해 바뀜 |
| 콘솔 포인터 | 화면 곳곳의 네 위치. 각각을 게스트 자신의 입력 장치에서 확인 |

**Windows Server 2022**(데스크톱 환경) 대상:

| | |
|---|---|
| 무인 설치 | 별도의 작은 ISO에 담은 `autounattend.xml`, 콘솔 입력 없음 |
| `guest_invoke_command` | 게스트에 네트워크 서비스가 생기기 전에 VMBus 위의 PowerShell Direct로 |
| SSH 부트스트랩 | OpenSSH 설치, 시작, 키 등록, 방화벽 설정을 전부 VMBus로 처리한 뒤 TCP로 접속 |
| `guest_copy_file` | VMBus로 호스트에서 게스트로 |
| `guest_copy_from` | VMBus로 게스트에서 호스트로. 크기와 SHA256 검증 |
| `set_guest_static_ip` | Windows 분기를, `Ethernet 2`라는 두 번째 어댑터에서 |
| 세션 브리지 | 같은 질의를 PowerShell Direct의 세션 0과 `guest_run_in_session`의 세션 1에서 각각 응답 |
| 권한 상승 | 그 세션에서 필터링되지 않은 관리자 토큰. `HKLM` 아래 쓰기로 증명 |
| 콘솔 캡처와 포인터 | 1024x768 데스크톱. 커서가 보낸 픽셀에 있음을 확인 |

**게스트가 아예 없는 상태**:

| | |
|---|---|
| 1세대와 2세대 | 양쪽 펌웨어 화면 캡처, 양쪽 모두 키 수용, 양쪽 모두 포인터를 이유와 함께 거부 |
| 중첩 가상화 | 정지된 VM에서 동적 메모리를 함께 끄며 활성화하고 `get_vm`으로 되읽기. 이어서 실행 중인 VM은 거부되고 손대지 않은 채로 유지 |
| 모든 VM 설정 | 각 항목을 같은 어휘로 쓰고 되읽음: 메모리, 프로세서, 부팅 순서, 보안 부팅, 자동 동작, 체크포인트 정책, 통합 서비스, vTPM, 콘솔 해상도, 시리얼 포트, 디스크 QoS와 배치, 어댑터 가드, 대역폭, VLAN 트렁크 |
| 두 펌웨어 계열 | 2세대에서는 장치 토큰으로 UEFI 부팅 순서 지정. 1세대에서는 이름 붙은 클래스 하나로 BIOS 순열을 완주했고, 보안 부팅과 vTPM은 합당한 이유로 거부됨 |

그리고 `rename_vm`: 자격 증명과 고정된 호스트 키가 새 이름 아래에서 모두 확인됐고,
키는 지문을 비교해서 확인했습니다. 고정이 사라지는 일은 조용히 벌어지기 때문에,
그러지 않으면 잘 옮겨진 경우와 구분되지 않습니다.

응답 파일은 일부러 OpenSSH를 설치하지 않습니다. 거기에 sshd를 넣었다면 부트스트랩
테스트가 아무것도 증명하지 못했을 것입니다. GUI 테스트는 픽셀이 아니라 자동화
트리를 검사합니다. 스크린샷은 어느 쪽이든 지나가며 저장해 두는데, 정말 뭔가
잘못됐을 때 게스트가 무엇을 보여 주고 있었는지에 대한 유일한 기록이기 때문입니다.
</details>

<details>
<summary><b>구현했지만 돌려 보지 않은 것</b></summary>

- **External 스위치 생성과 삭제.** 돌린다는 것은 멀쩡히 동작하는 컴퓨터의 네트워크를
  끊는다는 뜻이라, 콘솔 접근이 확보된 별도 세션으로 미뤄 두었습니다. 가드와
  preflight는 테스트돼 있고, 생성 자체는 아닙니다.
- **`export_vm` / `import_vm` / `resize_vhd` / `convert_vhd`.**
- **게스트 안에서 하이퍼바이저가 실제로 도는 것.** 측정한 것은 설정과 그 가드이지,
  WSL2나 Hyper-V가 거기서 부팅되는 것이 아닙니다. 그것을 하려면 그럴 수 있는 호스트
  CPU도 필요한데, Hyper-V 호스트는 자기 가상화 플래그를 false로 보고하므로 믿고
  제공할 만한 preflight가 없습니다.
- **위 두 가지 외의 게스트.** 다른 Windows 버전과 Linux 배포판도 구조상 동작해야
  합니다. 게스트 쪽 요구 사항이 두 계열 모두 트리에 포함해 배포하는 드라이버뿐이기
  때문입니다. 다만 그것은 기대이지 측정이 아닙니다.
</details>

<details>
<summary><b>결함이 아니라 환경의 한계인 것</b></summary>

- `guest_copy_file`은 Linux 6.10 이상에서 동작할 수 없습니다. `hypervfcopyd`가 붙는
  `/dev/vmbus/hv_fcopy` 장치가 제거됐기 때문입니다. 그런데도 데몬은 자신이 활성이라고
  보고합니다. 오류 메시지가 이 사실을 알려 주고 `ssh_exec`을 가리킵니다.
- Rocky 10에는 X 서버가 없습니다(RHEL 10이 걷어냈습니다). 그래서 Linux GUI 테스트는
  Wayland 컴포지터를 띄우고, 포인터 위치는 커널 입력 장치에서 읽습니다. Wayland는
  클라이언트에게 포인터가 어디 있는지 알려 주지 않기 때문입니다.
</details>

---

## 라이선스

MIT. [LICENSE](LICENSE)를 보세요.
