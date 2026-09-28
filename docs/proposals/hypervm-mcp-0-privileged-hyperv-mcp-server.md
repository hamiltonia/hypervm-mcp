# Privileged Hyper-V MCP Server with Network & Storage Control - Spec Proposal

| Item       | Detail                           |
|------------|----------------------------------|
| Author     | heavycaffeiner(Dong Hyun Kim)    |
| Created    | 2026-08-02                       |
| Status     | **Draft** / In Review / Approved |
| Reviewers  |                                  |

---

## 1. Summary

Hyper-V VM을 조작하려면 관리자 권한이 필요하지만, Claude Code와 같은 MCP 클라이언트는 비권한 사용자 세션에서 실행되므로 작업할 때마다 UAC 권한 상승 프롬프트가 발생한다. 본 프로포절은 Hyper-V 조작 권한을 상주하는 LocalSystem Windows 서비스로 격리하고, MCP 클라이언트는 비권한 stdio 브리지 프로세스를 통해 Named Pipe로 해당 서비스에 접속하는 Go 기반 MCP 서버를 제안한다. 서비스 설치 시 1회의 UAC 동의만 필요하며, 이후 어떤 워크스페이스에서 몇 개의 세션을 열든 권한 상승 프롬프트 없이 Hyper-V를 제어할 수 있다.

VM 제어에 더해 세 가지 접근 축을 제공한다. **(1) 게스트 실행** — PowerShell Direct와 SSH. **(2) 터널** — VM 내부 서비스를 호스트 `localhost` 또는 호스트의 Tailscale 주소에 바인딩하여 tailnet 전체로 노출하는 상주형 TCP 포워더. **(3) 네트워크 토폴로지** — External 가상 스위치를 통해 게스트를 물리 LAN의 1급 노드로 만들어, 호스트와 게스트가 각자의 IP(`192.168.0.x`)를 갖는 상태에서 SMB처럼 포워딩으로 대체 불가능한 프로토콜을 테스트할 수 있게 한다. 아울러 VM 구성 파일·VHD·체크포인트·내보내기 이미지의 저장 위치를 모두 명시적으로 지정할 수 있다.

## 2. Background & Motivation

### 2.1 현재 상황

- 현재 개발 워크플로우는 이 워크스페이스 외의 다른 프로젝트에서도 Hyper-V VM을 기동하고 그 위에서 작업하는 과정을 포함한다.
- Hyper-V의 관리 인터페이스(PowerShell `Hyper-V` 모듈, `root\virtualization\v2` WMI 네임스페이스)는 호출자가 관리자 권한을 갖거나 `Hyper-V Administrators` 그룹 멤버일 것을 요구한다.
- 실제로 본 환경(`HYUNDESKTOP\Hyun`)에서 비권한 셸로 `Get-WindowsOptionalFeature -Online` 호출 시 `The requested operation requires elevation` 오류가 발생함을 확인했다.
- 개발 대상 서비스는 VM 안에서 기동되지만, 이를 검증·소비하는 도구(브라우저, API 클라이언트, SMB 클라이언트, 다른 개발 머신)는 VM 밖에 있다.

### 2.2 문제점

#### 2.2.1 권한 상승

1. **반복적인 UAC 프롬프트** — MCP 서버를 `requireAdministrator` 매니페스트로 빌드하는 방식은, MCP 클라이언트가 세션마다 서버 프로세스를 새로 spawn하기 때문에 세션 시작마다 UAC 동의 창을 띄운다. 자동화된 에이전트 워크플로우가 사람의 클릭을 기다리며 멈춘다.
2. **워크스페이스 간 재현 불가** — 워크스페이스마다 개별적으로 권한 상승 설정을 반복해야 하며, 설정이 각 워크스페이스의 MCP 구성 파일에 흩어진다.
3. **비권한 프로세스와 권한 프로세스의 stdio 연결 제약** — 상승된 프로세스와 비상승 프로세스 간에는 UIPI(User Interface Privilege Isolation)로 인해 stdio 파이프 상속이 안정적으로 동작하지 않는다. 즉 "MCP 클라이언트가 상승된 자식 프로세스를 stdio로 붙잡는" 구성 자체가 구조적으로 취약하다.
4. **다중 세션 동시 접속 불가** — 프로세스별 상승 모델에서는 여러 Claude Code 인스턴스가 각각 별도의 상승 프로세스를 띄우며, Hyper-V 상태에 대한 동시 조작 조율 지점이 없다.

#### 2.2.2 VM 서비스 접근

5. **VM IP가 불안정하다** — Default Switch는 NAT 기반이며 호스트 재부팅 및 VM 재기동 시 서브넷과 할당 IP가 바뀐다. `http://172.30.x.y:8080`을 고정 주소로 삼을 수 없다.
6. **게스트 loopback 바인딩 서비스에 도달할 수 없다** — 게스트 내부에서 `127.0.0.1:5432`에만 바인딩된 데이터베이스처럼, 게스트 loopback에 묶인 서비스는 호스트에서 IP로 직접 접근하는 방법이 존재하지 않는다.
7. **tailnet에서 VM 서비스에 접근할 수 없다** — 호스트는 tailnet에 참여하지만 VM은 Hyper-V 가상 스위치 뒤의 별도 세그먼트에 있다. tailnet 피어 입장에서 VM은 라우팅 대상이 아니므로 도달 불가능하다.
8. **접근 경로 설정이 수작업이고 휘발성이다** — `netsh interface portproxy`는 재부팅을 넘어 남아 정리되지 않고, 임시 SSH 포워딩(`ssh -L`)은 터미널 세션이 끝나면 사라진다. 어느 쪽도 "에이전트가 열고, 조회하고, 닫는" 관리 대상이 되지 못한다.

#### 2.2.3 포트 포워딩으로 해결되지 않는 테스트 시나리오

9. **호스트가 이미 점유한 포트를 게스트도 써야 하는 경우가 있다.** 게스트 안의 서비스가 제공하는 SMB 기능을 테스트하려면 게스트의 445번 포트에 접근해야 한다. 그런데 Windows 호스트의 445번은 `LanmanServer`가 항상 점유하므로 `127.0.0.1:445` 터널은 반드시 `PORT_IN_USE`로 실패한다. **다른 호스트 포트로 매핑하는 우회도 성립하지 않는다** — SMB 클라이언트에는 포트를 지정하는 UNC 문법이 없다(`\\host:1445\share`는 유효하지 않다). RDP(3389), WinRM(5985), NetBIOS(137-139)도 같은 부류다.
10. **프로토콜이 IP·호스트명 정체성에 민감한 경우가 있다.** Kerberos SPN은 호스트명에 묶이고, SMB 서명·암호화는 세션 바인딩을 검증하며, 일부 서비스는 클라이언트의 소스 IP로 인가를 판단한다. NAT나 포트 포워딩은 이런 검증을 깨뜨리므로 "포워딩으로 접근은 되는데 인증이 실패하는" 진단하기 어려운 상태를 만든다.
11. **LAN의 다른 기기가 게스트에 직접 접속해야 하는 경우가 있다.** 터널은 항상 호스트를 경유하므로 호스트가 단일 장애점이자 대역폭 병목이 된다. 브로드캐스트·멀티캐스트에 의존하는 mDNS, NetBIOS 이름 해석, DHCP 자체는 터널을 통과하지 못한다.

    → 이 세 부류는 공통적으로 **게스트를 물리 LAN의 1급 노드로 만들어 호스트와 게스트가 각자의 `192.168.0.x` IP를 갖게 하는 것** 외에 답이 없다. 이는 터널의 대안이 아니라 별개의 축이며, 둘 다 필요하다.

#### 2.2.4 저장 위치 통제

12. **VM 파일이 어디에 놓이는지 통제할 수 없다.** Hyper-V 기본값은 시스템 드라이브 아래(`C:\ProgramData\Microsoft\Windows\Hyper-V\`, `C:\Users\Public\Documents\Hyper-V\`)다. 시스템 SSD 용량이 제한적인 환경에서 수십 GB짜리 VHDX가 여기에 쌓이면 곧 한계에 부딪힌다. VM 구성 파일, VHD, 체크포인트, 스마트 페이징 파일은 각각 별도의 경로 설정을 가지며, 특히 VM 구성 파일 경로는 **생성 후 변경할 수 없다.**
13. **골든 이미지 기반 프로비저닝 경로가 없다.** 매번 ISO로 OS를 설치하는 것은 수십 분이 걸린다. 완성된 VHDX를 부모로 삼는 차등 디스크(differencing disk)로 VM을 만들면 수 초 만에 프로비저닝되지만, 부모 이미지의 위치와 차등 디스크의 위치를 모두 지정할 수 있어야 한다.
14. **LocalSystem 서비스의 경로 접근성이 사용자 세션과 다르다.** 이는 위 두 문제를 해결하는 과정에서 반드시 걸리는 함정이다. 상세는 4.5.3에서 다룬다.

### 2.3 해결 방향

권한 경계를 **프로세스 생명주기(spawn 시점)** 가 아니라 **한 번 설치되는 서비스** 로 옮긴다. 권한 상승 비용을 설치 시점 1회로 상각(amortize)하고, 이후의 모든 접근은 OS가 관리하는 Named Pipe ACL로 인가한다.

같은 서비스 프로세스가 **터널 매니저**를 겸하게 한다. 서비스는 이미 상주하고 있고, 이미 관리자 권한을 갖고 있으며(방화벽 규칙 조작에 필요), 이미 Hyper-V에게 VM IP를 물어볼 수 있다. 터널을 서비스에 두면 MCP 세션이 끊겨도 터널이 살아 있고, VM 재기동으로 IP가 바뀌어도 서비스가 재해석할 수 있다.

그리고 **네트워크 토폴로지와 저장 위치를 1급 도구로 노출한다.** 터널이 풀 수 있는 문제와 풀 수 없는 문제를 도구 수준에서 구분하여, 에이전트가 상황에 맞는 축을 선택할 수 있게 한다.

## 3. Goals & Non-Goals

### 3.1 Goals

#### 3.1.1 권한 및 전송

- [ ] LocalSystem 권한으로 동작하는 상주형 Windows 서비스(`HyperVM-MCP`)를 구현하고, 설치·제거·기동·중지 CLI를 제공한다.
- [ ] 서비스 설치 시 1회의 UAC 동의 이후, MCP 클라이언트 사용 중에는 어떤 권한 상승 프롬프트도 발생하지 않도록 한다.
- [ ] Claude Code의 표준 `mcpServers` stdio 설정과 100% 호환되는 경량 브리지 프로세스(`hypervm-mcp.exe bridge`)를 제공한다.
- [ ] Named Pipe에 명시적 DACL을 부여하여, 설치 시점에 등록된 사용자 SID와 LocalSystem만 연결 가능하도록 한다.
- [ ] 여러 MCP 클라이언트 세션이 동시에 서비스에 접속할 수 있도록 한다.

#### 3.1.2 VM 제어

- [ ] VM 라이프사이클 도구 (조회, 시작, 종료, 재시작, 일시 중단/재개, 게스트 IP 대기)
- [ ] 체크포인트/스냅샷 도구 (생성, 조회, 적용, 삭제)
- [ ] VM 생성/삭제 및 하드웨어 구성 도구 (CPU, 메모리, VHD 연결)
- [ ] PowerShell Direct 기반 게스트 명령 실행 및 파일 전송 도구
- [ ] 게스트 OS 자격증명(암호 및 SSH 개인키)을 대화 컨텍스트에 노출하지 않고 DPAPI 머신 스코프로 보관하고 조회한다.

#### 3.1.3 저장 위치 및 이미지

- [ ] VM 생성 시 다음 경로를 각각 독립적으로 지정할 수 있게 한다: VM 구성 파일, VHD, 체크포인트, 스마트 페이징 파일.
- [ ] 호스트의 기본 VM 경로와 기본 VHD 경로를 조회·변경하는 도구를 제공한다.
- [ ] VHD 생성·연결·분리·크기 조정·최적화·형식 변환 도구를 제공하며, 모든 경로는 호출자가 지정한다.
- [ ] 골든 VHDX를 부모로 하는 차등 디스크 기반의 빠른 VM 프로비저닝 도구를 제공한다.
- [ ] VM 내보내기(`Export-VM`)와 가져오기(`Import-VM`) 도구를 제공하며, 내보내기 대상 디렉터리를 지정할 수 있게 한다.
- [ ] 모든 경로 인자에 대해 **LocalSystem 컨텍스트에서의 실제 접근 가능성**을 사전 검증하고, 실패 시 원인(매핑 드라이브, UNC 권한, 존재하지 않는 상위 디렉터리)을 구분해 보고한다.

#### 3.1.4 네트워크 토폴로지

- [ ] 가상 스위치 조회·생성·삭제 도구를 제공하며, External 스위치 생성 시 바인딩할 물리 어댑터를 선택할 수 있게 한다.
- [ ] 호스트의 물리 네트워크 어댑터 목록을 조회하는 도구를 제공한다.
- [ ] VM 네트워크 어댑터의 스위치 연결, 정적 MAC 주소, VLAN ID, MAC 주소 스푸핑 허용 여부를 설정하는 도구를 제공한다.
- [ ] 게스트 OS 내부에 정적 IP를 설정하는 도구를 제공한다(Windows 게스트는 PowerShell Direct, Linux 게스트는 NetworkManager/netplan).
- [ ] 시나리오에 맞는 토폴로지를 판단할 수 있도록, 현재 VM의 네트워크 도달성(호스트↔게스트, 게스트↔LAN, LAN↔게스트)을 진단하는 도구를 제공한다.

#### 3.1.5 SSH 및 터널

- [ ] 게스트 OS에 대한 SSH 명령 실행 도구를 제공한다. 암호 인증과 공개키 인증을 모두 지원하고, 호스트키는 TOFU(Trust On First Use)로 고정한다.
- [ ] 서비스에 상주하는 TCP 터널 매니저를 구현하고, 두 가지 데이터 경로 모드를 제공한다.
  - **direct 모드** — 서비스가 직접 `게스트IP:포트`로 TCP 프록시. 게스트 서비스가 `0.0.0.0`에 바인딩된 경우.
  - **ssh 모드** — 서비스가 게스트 sshd로 SSH 세션을 열고 그 안에서 채널 포워딩. 게스트 서비스가 게스트 `127.0.0.1`에만 바인딩된 경우에도 도달 가능.
- [ ] 터널 바인드 대상을 스코프로 추상화한다: `loopback`(기본), `tailnet`, `all`, 명시적 IP.
- [ ] `tailnet`/`all` 스코프 바인딩 시 Windows 방화벽 인바운드 허용 규칙을 자동 생성하고, 터널 종료 및 서비스 정지 시 정확히 그 규칙만 제거한다.
- [ ] `tailscale serve`를 터널 위에 얹어 tailnet 피어가 `https://<host>.<tailnet>.ts.net/`으로 접근할 수 있게 한다.
- [ ] 터널은 게스트 IP를 영구 캐시하지 않고 백엔드 연결 실패 시 재해석하여 VM 재기동에 대응한다.
- [ ] 터널 정의를 디스크에 영속화하고 서비스 재시작 시 복원한다.

### 3.2 Non-Goals

- [ ] **원격 Hyper-V 호스트 관리는 범위 밖이다.** 서비스가 설치된 로컬 호스트의 Hyper-V만 제어한다.
- [ ] **MCP의 HTTP/SSE 전송은 구현하지 않는다.** MCP 전송은 Named Pipe + stdio 브리지로 한정한다. (터널이 노출하는 HTTP는 VM 안의 서비스이지 MCP 서버가 아니다. 둘을 혼동해서는 안 된다.)
- [ ] **VM 이름 allowlist, 파괴적 작업 차단 등의 정책 엔진은 구현하지 않는다.** 개인 개발 머신을 대상으로 하므로 모든 도구를 무제한 노출하고, 위험 조작에 대한 승인은 MCP 클라이언트 자체의 도구 권한 프롬프트에 위임한다.
- [ ] **WMI(`root\virtualization\v2`) 직접 호출은 하지 않는다.** Hyper-V 조작 전 기능을 PowerShell `Hyper-V` 모듈 호출로 구현한다.
- [ ] **Windows 이외 플랫폼 지원은 하지 않는다.** 전 코드가 `windows` 빌드 태그 하에 놓인다.
- [ ] **게스트 OS 프로비저닝(패키지 설치, sshd 설치·설정, 애플리케이션 배포)은 범위 밖이다.** 명령 실행 원시 도구만 제공하며, sshd는 게스트에 이미 구성되어 있다고 가정한다. 예외는 정적 IP 설정(3.1.4)뿐이며, 이는 네트워크 토폴로지 변경과 불가분이라 포함한다.
- [ ] **Tailscale 자체의 설치·로그인·ACL 관리는 하지 않는다.** 호스트에 Tailscale이 설치되어 로그인된 상태를 전제하며, 서비스는 그 상태를 읽고 활용만 한다.
- [ ] **VM 안에 Tailscale을 설치하지 않는다.** VM을 tailnet에 직접 참여시키는 편이 근본적으로 더 나은 해법인 경우가 있으나, 그것은 게스트 프로비저닝이므로 본 범위 밖이다.
- [ ] **Tailscale Funnel(공개 인터넷 노출)은 제공하지 않는다.** `tailscale serve` 기반 tailnet 내부 노출까지만 다룬다.
- [ ] **Tailscale subnet router 설정은 자동화하지 않는다.** tailnet 관리 콘솔의 수동 승인과 호스트 IP 포워딩 활성화를 요구하며 부작용 범위가 넓다. `doctor`가 이 대안을 안내하되 실행하지는 않는다.
- [ ] **UDP 터널링은 하지 않는다.** TCP만 지원한다. (UDP가 필요한 시나리오는 External 스위치로 해결한다.)
- [ ] **호스트의 물리 네트워크 구성 자체는 변경하지 않는다.** External 스위치 생성 시 Hyper-V가 수행하는 NIC 재바인딩 외에, 호스트의 IP·라우팅·DNS 설정을 직접 건드리지 않는다.
- [ ] **VHD 내부 파일시스템은 조작하지 않는다.** 오프라인 VHD 마운트를 통한 파일 주입은 구현하지 않는다. 파일 전송은 `guest_copy_file` 또는 터널을 통한다.

## 4. Technical Design

### 4.1 Architecture Overview

```mermaid
flowchart TB
    subgraph Ext["외부"]
        PEER["Tailnet 피어"]
        LAN["LAN 기기<br/>192.168.0.50"]
    end

    subgraph UserSession["사용자 세션 (비권한, Medium IL)"]
        CC["Claude Code x N"]
        BR["hypervm-mcp.exe bridge x N"]
        APP["브라우저 / SMB 클라이언트"]
        CC -- "stdio JSON-RPC" --> BR
    end

    PIPE["Named Pipe \\.\pipe\hypervm-mcp<br/>DACL: SYSTEM + BA + 설치자 SID"]

    subgraph SystemSession["서비스 세션 (LocalSystem)"]
        MCP["MCP Server<br/>(연결당 1)"]
        subgraph Singletons["프로세스 전역 싱글턴"]
            PS["PowerShell Runner"]
            CRED["Credential Store"]
            TM["Tunnel Manager"]
            SSHP["SSH Pool"]
        end
    end

    subgraph HV["Hyper-V"]
        SW1["Default Switch (NAT)"]
        SW2["External Switch<br/>→ 물리 NIC"]
        VM1["VM-A 172.30.x.y<br/>0.0.0.0:8080<br/>127.0.0.1:5432"]
        VM2["VM-B 192.168.0.42<br/>:445 SMB"]
        SW1 --- VM1
        SW2 --- VM2
    end

    BR --> PIPE --> MCP
    MCP --> PS & CRED & TM & SSHP
    PS -- "제어" --> HV

    APP -- "① 127.0.0.1:8080" --> TM
    PEER -- "② 100.x.y.z:8080" --> TM
    TM -- "direct" --> VM1
    TM -- "ssh ch." --> SSHP --> VM1

    APP -- "③ \\192.168.0.42\share" --> VM2
    LAN -- "④ 직접" --> VM2
```

경로 ①②는 터널을 통한 접근(2.2.2 해결), ③④는 External 스위치를 통한 직접 접근(2.2.3 해결)이다. 두 경로는 대체 관계가 아니라 보완 관계다.

#### 4.1.1 핵심 설계 결정: 브리지는 프로토콜을 해석하지 않는다

브리지 프로세스는 MCP 프로토콜을 파싱하지 않는다. `os.Stdin`에서 읽은 바이트를 파이프로, 파이프에서 읽은 바이트를 `os.Stdout`으로 그대로 흘려보내는 양방향 `io.Copy`만 수행한다. MCP SDK의 `IOTransport`가 개행 구분 JSON을 사용하므로, 바이트 스트림을 그대로 중계하는 것만으로 프로토콜이 성립한다.

이 설계의 귀결:

- MCP 서버 구현 전체가 서비스 측에 존재한다. 도구를 추가할 때 브리지를 재빌드할 필요가 없다.
- 브리지는 MCP 스펙 버전 변화에 영향받지 않는다.
- 브리지 프로세스가 신뢰 경계 밖(비권한 세션)에 있으므로, 브리지에 프로토콜 상태를 두지 않는 편이 보안상으로도 옳다. 인가는 전적으로 파이프 DACL이 담당한다.

#### 4.1.2 핵심 설계 결정: 터널은 MCP 세션보다 오래 산다

터널 매니저는 연결별 MCP 서버 인스턴스가 아니라 **서비스 프로세스 전역**에 하나 존재한다.

1. **의미론적으로 옳다.** 사용자가 "VM의 8080을 localhost로 열어줘"라고 했을 때 기대하는 수명은 "이 대화가 끝날 때까지"가 아니라 "내가 닫을 때까지"다.
2. **포트는 머신 전역 자원이다.** 세션마다 별도 터널 매니저를 두면 두 세션이 같은 호스트 포트를 점유하려 할 때 조정 지점이 없다.
3. **재시작 복원이 가능해진다.** 전역 상태이므로 디스크에 직렬화하고 서비스 시작 시 복원할 수 있다.

#### 4.1.3 세션 격리

서비스는 파이프 인스턴스마다 독립적인 MCP 서버 인스턴스를 생성한다. `initialize` 핸드셰이크와 프로토콜 상태는 연결별로 격리된다. 반면 PowerShell Runner, Credential Store, Tunnel Manager, SSH Client Pool은 프로세스 전역 싱글턴으로 모든 연결이 공유한다.

### 4.2 Data Model Changes

DB 스키마 변경 없음. 다음 5종의 온디스크 상태를 새로 도입한다. 모두 `%ProgramData%\HyperVM-MCP\` 아래에 위치하며, 이 디렉터리는 상속을 끊고 LocalSystem(Full) + Administrators(Full) + 설치자 SID(Read)의 명시적 DACL을 갖는다.

#### 4.2.1 `config.json` — 설치 상태

```json
{
  "version": 1,
  "pipe_name": "hypervm-mcp",
  "allowed_sid": "S-1-5-21-XXXXXXXXXX-XXXXXXXXXX-XXXXXXXXXX-1001",
  "powershell_path": "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
  "powershell_timeout_seconds": 300,
  "max_concurrent_powershell": 8,
  "tailscale_path": "C:\\Program Files\\Tailscale\\tailscale.exe",
  "image_library_path": "",
  "log_level": "info"
}
```

`allowed_sid`는 설치를 실행한 사용자의 SID를 자동으로 채운다. 이 값이 파이프 DACL 생성의 유일한 입력이다. `image_library_path`는 골든 VHDX와 ISO를 찾을 기본 디렉터리로, 비어 있으면 이미지 관련 도구가 절대 경로를 요구한다.

#### 4.2.2 `credentials.dat` — 게스트 자격증명

- **암호화**: 파일 전체를 `CryptProtectData`에 `CRYPTPROTECT_LOCAL_MACHINE` 플래그로 암호화한다. 사용자 스코프 DPAPI를 쓰지 않는 이유는, 자격증명을 등록하는 주체는 사용자 세션의 CLI지만 복호화하는 주체는 LocalSystem 서비스이기 때문이다. 두 컨텍스트가 다르므로 머신 스코프여야 한다.

평문 구조(암호화 전):

```json
{
  "version": 1,
  "entries": {
    "Dev-Win11": {
      "username": "admin",
      "password": "...",
      "ssh_port": 22,
      "ssh_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n...",
      "ssh_key_passphrase": ""
    }
  }
}
```

키는 VM 이름이다. `password`는 PowerShell Direct와 SSH 암호 인증에 공용으로 쓰인다. `ssh_private_key`가 존재하면 SSH는 공개키 인증을 우선 시도하고 실패 시 암호로 폴백한다.

#### 4.2.3 `known_hosts.json` — SSH 호스트키 고정

```json
{
  "version": 1,
  "hosts": {
    "Dev-Ubuntu": {
      "key_type": "ssh-ed25519",
      "fingerprint_sha256": "SHA256:abc123...",
      "public_key": "AAAAC3NzaC1lZDI1...",
      "first_seen": "2026-08-02T10:00:00Z"
    }
  }
}
```

키는 **VM 이름**이지 IP가 아니다. Default Switch 환경에서 VM IP는 재기동마다 바뀌므로 IP를 신뢰 앵커로 삼으면 매번 키가 새로 보이고 TOFU가 무의미해진다. VM 이름은 Hyper-V 호스트 내에서 안정적인 식별자다.

#### 4.2.4 `tunnels.json` — 터널 정의 영속화

```json
{
  "version": 1,
  "next_id": 3,
  "tunnels": [
    { "id": "tnl-1", "vm_name": "Dev-Ubuntu", "mode": "direct",
      "bind_scope": "tailnet", "host_port": 8080, "guest_port": 8080,
      "auto_restore": true, "created": "2026-08-02T10:00:00Z" },
    { "id": "tnl-2", "vm_name": "Dev-Ubuntu", "mode": "ssh",
      "bind_scope": "loopback", "host_port": 5432, "guest_port": 5432,
      "auto_restore": true, "created": "2026-08-02T10:05:00Z" }
  ]
}
```

**정의만 영속화하고 런타임 상태(해석된 게스트 IP, 실제 바인드된 주소, 활성 커넥션 수, 전송 바이트)는 저장하지 않는다.** 게스트 IP는 재기동 시 무효가 되므로 저장하면 오히려 해롭다. 복원 시 IP는 처음부터 다시 해석한다.

#### 4.2.5 `logs\service.log` — 로그

서비스는 stderr가 없으므로 파일 로깅과 Windows 이벤트 로그(`Application` 소스 `HyperVM-MCP`, 경고 이상만)를 병행한다.

**자격증명은 어떤 로그 레벨에서도 기록하지 않는다.** 자격증명이 포함된 PowerShell 스크립트는 본문을 `<redacted>`로 대체하고, SSH 개인키와 암호는 어떤 경로로도 로그에 진입하지 않는다. 게스트 명령 본문도 기록하지 않는다(명령줄에 토큰이 섞이는 경우가 흔하다).

### 4.3 Core Logic — 권한, 전송, 실행

#### 4.3.1 설치 시퀀스

`hypervm-mcp.exe service install`은 상승된 컨텍스트를 요구한다. 비상승 상태로 실행되면 `ShellExecuteEx`의 `runas` 동사로 자기 자신을 재실행하여 UAC 프롬프트를 1회 발생시킨다. 이것이 전체 시스템에서 유일하게 UAC가 관여하는 지점이다.

1. **호출자 SID 확보** — 재실행 전에 현재(비상승) 토큰의 사용자 SID를 확정하여 `--allowed-sid` 인자로 넘긴다. UAC 분할 토큰 특성상 대개 상승 후에도 SID가 같지만, 다른 관리자 계정으로 상승하면 달라지므로 재실행 전 값을 신뢰한다.
2. **디렉터리 생성 및 ACL 설정** — `%ProgramData%\HyperVM-MCP\`(및 `bin`, `logs`)를 생성하고 상속을 끊은 뒤 명시적 DACL을 부여한다.
3. **`config.json` 기록** — 4.2.1의 내용. `tailscale_path`는 설치 시점에 탐지하여 채우고, 없으면 빈 문자열로 둔다(런타임 재탐지).
4. **바이너리 배치** — 현재 실행 중인 exe를 `%ProgramData%\HyperVM-MCP\bin\hypervm-mcp.exe`로 복사한다. 서비스가 사용자의 프로젝트 디렉터리를 `ImagePath`로 참조하면, 해당 디렉터리가 비권한 사용자에 의해 쓰기 가능할 경우 서비스 바이너리 교체를 통한 권한 상승 취약점이 된다. 반드시 ACL이 통제된 위치로 복사해야 한다.
5. **서비스 등록** — `OpenSCManager` → `CreateService`.
   - 이름 `HyperVM-MCP`, 시작 유형 `SERVICE_AUTO_START`, 계정 `LocalSystem`
   - `ImagePath`: `"%ProgramData%\HyperVM-MCP\bin\hypervm-mcp.exe" service run` (경로에 공백이 있으므로 따옴표 필수 — unquoted service path 취약점 회피)
   - 의존성: `vmms`(Hyper-V Virtual Machine Management). VMMS보다 먼저 뜨면 초기 조회가 전부 실패한다.
   - 복구 정책: 실패 시 재시작 (1차 5초, 2차 10초, 이후 60초)
6. **서비스 시작** — `StartService` 후 `SERVICE_RUNNING` 도달까지 최대 30초 폴링.
7. **결과 출력** — 워크스페이스의 `.mcp.json`에 붙여넣을 설정 스니펫을 출력한다.

멱등성: 이미 등록된 서비스가 존재하면 재등록 대신 `ChangeServiceConfig`로 `ImagePath`를 갱신하고, `config.json`을 덮어쓴 뒤 서비스를 재시작한다.

#### 4.3.2 파이프 DACL 구성

서비스는 시작 시 `config.json`의 `allowed_sid`를 읽어 다음 SDDL을 조립하고, `winio.ListenPipe`의 `SecurityDescriptor`로 지정한다.

```
D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019b;;;<allowed_sid>)
```

| ACE | 대상 | 권한 |
|-----|------|------|
| `(A;;GA;;;SY)` | LocalSystem | GENERIC_ALL |
| `(A;;GA;;;BA)` | BUILTIN\Administrators | GENERIC_ALL |
| `(A;;0x12019b;;;<sid>)` | 설치자 SID | 읽기/쓰기/속성 조회 + SYNCHRONIZE |

`D:P`의 `P`(PROTECTED)가 핵심이다. 이 플래그가 상속 ACE를 차단하므로 `Everyone`이나 `Authenticated Users`가 흘러들어오지 않는다. 명시된 3개 주체 외에는 `CreateFile`이 `ERROR_ACCESS_DENIED`로 실패한다.

`0x12019b`는 `FILE_GENERIC_READ | FILE_GENERIC_WRITE | SYNCHRONIZE`에서 파이프 클라이언트에 불필요한 비트를 제거한 마스크다. 클라이언트에게 `WRITE_DAC`/`WRITE_OWNER`를 주지 않는 것이 요점이다. 이를 주면 클라이언트가 스스로 DACL을 재작성할 수 있다.

#### 4.3.3 연결 처리 루프

```
서비스 시작
  ├─ config.json 로드
  ├─ 전역 싱글턴 구성: PowerShell Runner, Credential Store, SSH Pool, Tunnel Manager
  ├─ Tunnel Manager: tunnels.json 로드 → auto_restore 터널 재개시 (실패는 로그만, 서비스는 계속)
  ├─ SDDL 조립 → winio.ListenPipe(pipePath, &PipeConfig{SecurityDescriptor: sddl})
  ├─ SCM에 SERVICE_RUNNING 보고
  └─ for {
        conn, err := listener.Accept()
        if errors.Is(err, winio.ErrPipeListenerClosed) { break }   // 정상 종료
        if err != nil { log; continue }                             // 일시 오류는 루프 유지
        go handleConnection(conn)
     }

handleConnection(conn):
  ├─ defer conn.Close()
  ├─ 첫 줄 peek → 제어 프레임({"op":...}) vs MCP 프레임({"jsonrpc":...}) 라우팅
  ├─ MCP 경로:
  │    srv := mcp.NewServer(impl, opts)
  │    registerAllTools(srv, deps)                  // deps는 전역 싱글턴 참조
  │    srv.Run(ctx, &mcp.IOTransport{Reader: peeked, Writer: conn})
  └─ 제어 경로: handleControlOp(op) → JSON 1줄 응답 → 종료
```

`ctx`는 `SERVICE_CONTROL_STOP`에 연결된 취소 가능 컨텍스트다. 정지 시 리스너를 닫고 컨텍스트를 취소하여 진행 중인 연결, PowerShell 자식 프로세스, 터널 리스너가 모두 정리된다. **서비스 정지 시 생성했던 방화벽 규칙은 반드시 제거한다.** 규칙을 남기면 다음 부팅 때 아무 것도 리스닝하지 않는 포트에 대한 허용 규칙이 방치된다.

#### 4.3.4 브리지 동작

```
hypervm-mcp.exe bridge
  ├─ conn, err := winio.DialPipeContext(ctx, `\\.\pipe\hypervm-mcp`)
  │    실패 시 최대 10초간 200ms 간격 재시도 (서비스 기동 직후 레이스 대응)
  │    최종 실패 시 stderr에 진단 메시지 출력 후 exit 1
  ├─ go func() { io.Copy(conn, os.Stdin);  conn.CloseWrite() }()   // 클라이언트 → 서비스
  ├─ io.Copy(os.Stdout, conn)                                       // 서비스 → 클라이언트 (블로킹)
  └─ 어느 방향이든 EOF 도달 시 정리 후 종료
```

브리지의 stderr는 MCP 클라이언트가 진단 로그로 취급하므로, 연결 실패 사유를 구분해 사람이 읽을 수 있는 문장으로 출력한다.

#### 4.3.5 PowerShell Runner

모든 Hyper-V 조작은 단일 실행 경로를 통과한다.

```
Run(ctx, script string, args map[string]any) (json.RawMessage, error)
  1. 인자 바인딩: 스크립트 본문에 값을 문자열 연결하지 않는다.
     값은 JSON으로 직렬화하여 stdin으로 주입하고, 스크립트는 $P 변수로 참조한다.
     → PowerShell 인젝션 차단. VM 이름이나 경로에 "; Remove-Item C:\ -Recurse" 가
       들어가도 그것은 단지 문자열 값일 뿐 코드로 해석되지 않는다.

  2. 래핑:
       $ErrorActionPreference = 'Stop'
       $ProgressPreference    = 'SilentlyContinue'   # 진행률 바가 stdout을 오염시키는 것 방지
       $raw = [Console]::In.ReadToEnd()
       $P = if ($raw) { $raw | ConvertFrom-Json } else { $null }
       try {
         $result = & { <사용자 스크립트> }
         @{ ok = $true; data = $result } | ConvertTo-Json -Depth 6 -Compress
       } catch {
         @{ ok = $false; error = $_.Exception.Message
            category = $_.CategoryInfo.Category.ToString()
            fqid = $_.FullyQualifiedErrorId } | ConvertTo-Json -Compress
       }

  3. 실행: exec.CommandContext(ctx, pwshPath,
              "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
              "-EncodedCommand", base64(utf16le(wrapped)))
     stdin ← args의 JSON 직렬화
     타임아웃: config의 powershell_timeout_seconds (기본 300초)
     동시 실행: 세마포어로 max_concurrent_powershell(기본 8)개 제한

  4. 파싱: stdout을 봉투 구조로 언마셜.
     ok=true  → data 반환
     ok=false → error/category/fqid를 5-2의 오류 코드로 매핑
     언마셜 실패 → stdout 원문(최대 4KB 절단)을 포함한 INTERNAL 오류
```

**설계 근거 — 왜 봉투(envelope)를 씌우는가.** PowerShell은 실패를 두 가지 방식으로 표현한다. 종료 오류(terminating error)는 exit code에 반영되지만, 비종료 오류(non-terminating error)는 stderr에만 나타나고 exit code는 0으로 남는다. exit code만 보는 구현은 후자를 성공으로 오인한다. `$ErrorActionPreference = 'Stop'`으로 모든 오류를 종료 오류로 승격시키고, 성공/실패를 stdout의 구조화된 필드로 명시하여 이 모호성을 제거한다.

**설계 근거 — 왜 `-EncodedCommand`인가.** `-Command`에 문자열을 넘기면 Windows의 명령줄 파싱(`CommandLineToArgvW`)과 PowerShell의 자체 파싱을 연속으로 거치며 따옴표·백틱·앰퍼샌드의 이스케이프 규칙이 중첩된다. 경로 인자를 다루는 도구가 많은 본 설계에서 이는 특히 위험하다. `-EncodedCommand`는 base64로 인코딩된 UTF-16LE 바이트를 그대로 스크립트 본문으로 취급하므로 이 계층이 사라진다.

#### 4.3.6 게스트 IP 해석

터널과 SSH의 공통 전제다.

```
ResolveGuestIP(ctx, vmName) (addr, allAddrs, error)
  1. Get-VM 으로 상태 확인. Running이 아니면 VM_WRONG_STATE.
  2. Get-VMNetworkAdapter -VMName $P.name | Select-Object -ExpandProperty IPAddresses
     → 통합 서비스(Data Exchange)가 게스트로부터 수집해 보고하는 주소 목록.
  3. 필터링 우선순위:
       a. IPv4 이면서 link-local(169.254.0.0/16)이 아니고 loopback이 아닌 것
       b. (a)가 없으면 IPv6 중 link-local(fe80::/10)이 아닌 것
       c. 둘 다 없으면 GUEST_IP_UNAVAILABLE
  4. 여러 개면 첫 번째를 쓰되, 전체 목록을 함께 반환하여 호출자가 판단할 수 있게 한다.
     (External 스위치 + Default Switch 를 동시에 붙인 VM은 두 대역의 주소를 모두 보고한다.)
```

**캐싱하지 않는다.** VM 재기동이나 DHCP 갱신으로 IP가 바뀌므로, 터널은 백엔드 연결이 실패할 때마다 이 함수를 다시 호출한다(4.6.2 참조).

`wait_for_guest_ip` 도구는 위를 폴링한다(1초 간격, 기본 120초 타임아웃). VM은 부팅 후 통합 서비스가 올라오기까지 수십 초간 IP를 보고하지 않으므로, `start_vm` 직후 터널을 열거나 SSH로 붙으려면 이 대기가 필요하다.

### 4.4 Core Logic — 네트워크 토폴로지

#### 4.4.1 스위치 유형별 도달성

| 스위치 유형 | 게스트 IP 출처 | 호스트→게스트 | 게스트→LAN | LAN→게스트 | 게스트가 LAN에서 보이는가 |
|---|---|---|---|---|---|
| **Default Switch** | Hyper-V 내장 DHCP (`172.x.y.z`, 재부팅 시 대역 변경) | O | O (NAT) | **X** | X |
| **Internal** | 사용자 DHCP 또는 APIPA | O | X | X | X |
| **Private** | 사용자 DHCP 또는 APIPA | **X** | X | X | X |
| **External** | LAN의 DHCP 서버 (`192.168.0.x`) | O | O | **O** | **O** |

#### 4.4.2 판단 기준: 터널인가 External 스위치인가

**터널로 충분한 경우** — 게스트 서비스에 호스트나 tailnet 피어가 클라이언트로서 접속하기만 하면 되고, 프로토콜이 포트 번호에 유연하며(HTTP, gRPC, PostgreSQL 등 모두 임의 포트 지정 가능), 호스트가 해당 포트를 점유하고 있지 않은 경우.

**External 스위치가 필요한 경우** — 2.2.3의 세 부류 중 하나라도 해당하면 터널은 답이 아니다.

1. **호스트와 포트가 충돌한다.** SMB 445가 대표적이다. 호스트의 `LanmanServer`가 445를 점유하므로 터널 생성 자체가 `PORT_IN_USE`로 실패한다. 다른 포트로 매핑해도 SMB 클라이언트가 `\\host\share` 문법에서 포트를 지정할 수 없어 무의미하다.
2. **프로토콜이 IP·호스트명 정체성에 민감하다.** Kerberos SPN, SMB 세션 바인딩, 소스 IP 기반 인가.
3. **LAN의 다른 기기가 게스트에 직접 접속해야 한다.** 또는 브로드캐스트/멀티캐스트(mDNS, NetBIOS, DHCP)에 참여해야 한다.

에이전트가 이 판단을 내릴 수 있도록 `diagnose_vm_network` 도구(5-1-8)가 현재 토폴로지와 도달성을 구조화된 형태로 보고하고, `PORT_IN_USE` 오류의 `detail`에는 445·3389·5985 같은 알려진 시스템 점유 포트일 경우 External 스위치를 권하는 안내를 담는다.

#### 4.4.3 External 스위치 생성의 부작용

`New-VMSwitch -SwitchType External -NetAdapterName <NIC>`는 해당 물리 NIC을 Hyper-V 가상 스위치 확장 프로토콜에 재바인딩한다. 그 결과:

- **호스트의 네트워크 연결이 수 초간 끊긴다.** 원격 데스크톱이나 SSH로 호스트에 접속 중이라면 세션이 끊어진다. 도구는 이 사실을 결과의 `warnings`에 항상 포함하고, 실행 전 `confirm_disruption: true`를 요구한다.
- **`-AllowManagementOS $true`(기본값)이면** 호스트도 같은 NIC을 통해 계속 통신한다. 호스트에는 새 가상 어댑터(`vEthernet (<switch name>)`)가 생기고 원래 IP 설정이 그리로 이전된다. 이 값을 `$false`로 두면 호스트는 해당 NIC으로 통신할 수 없게 되므로, NIC이 하나뿐인 머신에서는 절대 사용해서는 안 된다. 도구는 물리 NIC 개수를 세어 1개일 때 `$false` 요청을 `INVALID_ARGUMENT`로 거부한다.
- **무선 어댑터에 External 스위치를 만들면** 브리징 대신 ARP 프록시 방식으로 동작하며, 게스트가 LAN에서 독립 IP로 보이지 않는 경우가 있다. 무선 NIC이 선택되면 `warnings`에 명시하고 유선 NIC 사용을 권한다.

#### 4.4.4 게스트 IP 확정 전략

External 스위치에서 게스트가 안정적인 `192.168.0.x`를 갖게 하는 방법은 세 가지이며, 도구는 셋 다 지원한다.

| 전략 | 방법 | 장점 | 단점 |
|---|---|---|---|
| **DHCP 임의 할당** | 아무 설정 없음 | 즉시 동작 | 주소가 바뀔 수 있음 |
| **MAC 고정 + 라우터 DHCP 예약** | `set_vm_network`로 정적 MAC 지정 후 라우터에서 예약 | 게스트 OS를 건드리지 않음, 재설치해도 유지 | 라우터 설정이 수동 |
| **게스트 정적 IP** | `set_guest_static_ip` | 완전 결정적 | 게스트 OS별 구현 필요, 오설정 시 게스트 고립 |

**`set_guest_static_ip`의 OS별 경로.** 이 도구는 게스트 네트워크를 끊을 수 있으므로 반드시 게스트 네트워크에 의존하지 않는 채널로 실행해야 한다.

- **Windows 게스트** — PowerShell Direct로 실행한다. 네트워크가 필요 없으므로 안전하다.
  `New-NetIPAddress -InterfaceIndex <idx> -IPAddress .. -PrefixLength .. -DefaultGateway ..` + `Set-DnsClientServerAddress`. 기존 주소는 `Remove-NetIPAddress`로 먼저 제거한다.
- **Linux 게스트** — PowerShell Direct가 없으므로 SSH를 쓸 수밖에 없고, 따라서 **적용 순간 자기 자신의 연결이 끊긴다.** 이를 다루기 위해 명령을 `nohup ... &`로 백그라운드화하고 즉시 세션을 닫은 뒤, 새 주소로 재접속을 폴링해 성공 여부를 판정한다. 배포판 감지:
  - `nmcli` 존재 → NetworkManager 경로 (`nmcli con mod ... ipv4.method manual`)
  - `/etc/netplan/` 존재 → netplan YAML 생성 후 `netplan apply`
  - 둘 다 없으면 `UNSUPPORTED_GUEST_OS`와 함께 **수동 실행용 명령 문자열을 반환한다.** 추측해서 잘못된 설정을 밀어넣지 않는다.
- 어느 경로든 실패 시 게스트가 네트워크에서 고립될 수 있으므로, 도구는 실행 전 체크포인트 생성을 권하고 `auto_checkpoint: true` 옵션으로 자동화한다.

### 4.5 Core Logic — 저장 위치 및 이미지

#### 4.5.1 Hyper-V가 관리하는 경로

VM 하나는 최소 네 종류의 파일 집합을 갖고, 각각 다른 경로 설정을 따른다.

| 대상 | 설정 방법 | 생성 후 변경 |
|---|---|---|
| VM 구성 파일 (`.vmcx`, `.vmrs`, `.vmgs`) | `New-VM -Path <dir>` | **불가.** 변경하려면 내보내기 후 다시 가져와야 한다 |
| 가상 하드 디스크 (`.vhdx`) | `New-VM -NewVHDPath` 또는 `Add-VMHardDiskDrive -Path` | 가능 (분리 → 이동 → 재연결) |
| 체크포인트 파일 (`.avhdx`) | `Set-VM -SnapshotFileLocation <dir>` | 가능 (기존 체크포인트가 없을 때) |
| 스마트 페이징 파일 (`.slp`) | `Set-VM -SmartPagingFilePath <dir>` | 가능 |
| 호스트 기본값 | `Set-VMHost -VirtualMachinePath -VirtualHardDiskPath` | 가능 (이후 생성분에만 적용) |

**VM 구성 파일 경로가 불변이라는 점이 설계상 중요하다.** `create_vm`은 `vm_path`를 받아야 하며, 이를 나중에 고칠 수 있다고 가정하는 API를 만들어서는 안 된다. 도구 설명에 이 제약을 명시한다.

#### 4.5.2 골든 이미지와 차등 디스크

```
create_vm_from_template(name, parent_vhd, vm_path, vhd_path, ...):
  1. parent_vhd 존재 및 접근성 검증 (4.5.3)
  2. 부모 디스크가 읽기 전용인지 확인. 아니면 warnings 에 담는다.
     차등 디스크의 부모가 변경되면 모든 자식이 손상되므로, 골든 이미지는
     읽기 전용이어야 한다. 도구는 강제하지 않고 경고만 한다 —
     같은 부모를 여러 VM이 공유하는 정상적인 워크플로우를 막지 않기 위함이다.
  3. New-VHD -Path <vhd_path> -ParentPath <parent_vhd> -Differencing
  4. New-VM -Name -Path <vm_path> -VHDPath <vhd_path> -Generation <부모와 일치>
     ※ 부모 VHDX가 GPT/UEFI 이미지면 Generation 2 여야 한다. Get-VHD 로
       부모의 파티션 스타일을 직접 알 수는 없으므로, generation 인자를
       명시하지 않으면 2를 기본으로 하되 warnings 에 확인을 요청한다.
  5. Set-VM -SnapshotFileLocation / -SmartPagingFilePath (지정된 경우)
  6. 하드웨어 설정 및 스위치 연결
```

차등 디스크 VM은 부모 이미지의 SID·호스트명·SSH 호스트키를 그대로 물려받는다. 여러 대를 동시에 띄우면 이름 충돌이 발생하고, SSH TOFU가 **의도적으로** 불일치를 보고한다(4.2.3의 키가 VM 이름이므로 새 VM은 새 엔트리가 되어 오탐은 없지만, 서로 다른 VM이 같은 호스트키를 갖는 상태가 된다). 도구는 이 사실을 `warnings`로 알리고, Windows 게스트라면 `sysprep` 실행을, Linux라면 `ssh-keygen -A` 재생성과 `machine-id` 재설정을 권한다. 자동 수행하지는 않는다(3.2의 게스트 프로비저닝 제외 원칙).

#### 4.5.3 LocalSystem 컨텍스트의 경로 접근성

**이것이 저장 위치 기능에서 가장 흔한 실패 원인이므로 모든 경로 인자에 사전 검증을 강제한다.**

서비스는 LocalSystem으로 동작하며, 이는 사용자 세션과 다음 차이를 갖는다.

| 경로 형태 | LocalSystem에서 | 처리 |
|---|---|---|
| 로컬 고정 드라이브 (`D:\VMs`) | 접근 가능 | 정상 |
| 매핑된 네트워크 드라이브 (`Z:\VMs`) | **접근 불가.** 드라이브 매핑은 로그온 세션별 오브젝트이며 LocalSystem 세션에는 존재하지 않는다 | `PATH_NOT_ACCESSIBLE`, UNC 경로 사용을 안내 |
| UNC 경로 (`\\NAS\vms`) | 머신 계정(`HYUNDESKTOP$`)으로 인증한다. 공유 권한에 머신 계정이 없으면 거부 | `PATH_NOT_ACCESSIBLE`, 공유 ACL에 `DOMAIN\HOST$` 추가를 안내 |
| 사용자 프로필 (`C:\Users\Hyun\VMs`) | 접근 가능하나, VM 파일은 VMMS와 VM별 가상 머신 SID(`NT VIRTUAL MACHINE\<GUID>`)도 접근해야 한다. 프로필 디렉터리의 상속 정책 때문에 Hyper-V의 자동 ACL 부여가 실패할 수 있다 | 경고 후 진행. 실패 시 원인 안내 |
| 존재하지 않는 상위 디렉터리 | 생성 실패 | `create_parents: true`면 생성, 아니면 `PATH_NOT_FOUND` |

```
ValidatePath(path, mode) error       // mode ∈ {read, write, create}
  1. 절대 경로인가? 아니면 INVALID_ARGUMENT (상대 경로의 기준 디렉터리가
     LocalSystem에서는 %SystemRoot%\System32 이므로 의미가 없다)
  2. 드라이브 문자 경로인 경우: GetDriveType 으로 판별.
       DRIVE_REMOTE → 매핑 드라이브. PATH_NOT_ACCESSIBLE + UNC 안내.
  3. UNC 경로인 경우: 서버 부분에 접근 시도. 실패 시 PATH_NOT_ACCESSIBLE +
     머신 계정 권한 안내.
  4. mode 별 실제 검증:
       read   → os.Stat
       write  → os.Stat + 임시 파일 생성/삭제로 쓰기 권한 확인
       create → 상위 디렉터리에 대해 write 검증. create_parents 면 MkdirAll.
  5. 성공 시 정규화된 경로 반환 (\\?\ 접두사 없이, 후행 구분자 제거)
```

`doctor`는 호스트 기본 VM 경로·기본 VHD 경로·`image_library_path`에 대해 이 검증을 실행하고 결과를 보고한다.

### 4.6 Core Logic — SSH 및 터널

#### 4.6.1 SSH 클라이언트

```
Connect(ctx, vmName, opts) (*ssh.Client, error)
  1. 자격증명 해석 (우선순위):
       a. 호출 인자의 username/password/private_key
       b. credentials.dat 의 vmName 엔트리
       c. 둘 다 없으면 CREDENTIAL_NOT_FOUND (추측하지 않는다)
  2. 대상 주소: ResolveGuestIP(vmName) + 엔트리의 ssh_port (기본 22)
  3. 인증 방법 배열 (순서대로 시도):
       - private_key 존재 시 ssh.PublicKeys(signer)
       - password 존재 시 ssh.Password(password)
       둘 다 없으면 INVALID_ARGUMENT
  4. HostKeyCallback = TOFU:
       known_hosts.json 에서 vmName 조회
         - 없음   → 기록하고 통과. 결과에 fingerprint 를 실어 호출자에게 알린다.
         - 일치   → 통과
         - 불일치 → SSH_HOST_KEY_MISMATCH.
                    trust_new_key=true 이면 덮어쓰고 통과.
                    (VM 재생성 시 정당하게 발생하므로 탈출구가 필요하다.)
  5. 커넥션 풀: vmName 키로 *ssh.Client 재사용. singleflight 로 중복 수립 방지.
     30초 간격 keepalive(SendRequest("keepalive@openssh.com"))로 사망 감지.
     사망 시 풀에서 제거하고 다음 요청에서 재연결한다.
```

#### 4.6.2 터널 매니저

**터널 열기**

```
Open(ctx, spec) (*Tunnel, error)
  1. 검증: mode ∈ {direct, ssh}, guest_port ∈ [1,65535]
  2. 바인드 주소 해석:
       loopback → ["127.0.0.1"]
       tailnet  → TailscaleIPs()  (없으면 TAILSCALE_UNAVAILABLE / TAILSCALE_NOT_RUNNING)
       all      → ["0.0.0.0", "::"]
       그 외    → IP 리터럴 파싱, 실패 시 INVALID_ARGUMENT
  3. host_port 가 0이면 OS에게 임의 포트를 받는다(:0 리스닝 후 실제 포트 확인).
  4. 각 바인드 주소마다 net.Listen("tcp", addr:port).
     EADDRINUSE → PORT_IN_USE.
       host_port 가 445/3389/5985/139/137 등 알려진 시스템 점유 포트면
       detail 에 "이 포트는 터널로 노출할 수 없다. External 스위치를 쓰라"는
       안내를 담는다(4.4.2).
     이미 성공한 리스너는 전부 닫고 롤백한다(부분 성공 금지).
  5. scope != loopback 이면 방화벽 규칙 생성:
       New-NetFirewallRule -Name "HyperVM-MCP-<id>" -DisplayName "HyperVM-MCP <id>"
         -Direction Inbound -Action Allow -Protocol TCP -LocalPort <host_port>
         [-LocalAddress <tailscale ip>]        # tailnet 스코프일 때만 주소 제한
     실패는 치명적으로 다루지 않고 경고 로그 + warnings 필드에 담는다.
  6. mode == ssh 이면 이 시점에 SSH 연결을 1회 수립해 자격증명·도달성을 즉시 검증한다.
     지연 연결로 미루면 실패가 첫 접속 시점까지 숨는다.
  7. accept 루프 goroutine 기동, tunnels.json 갱신, Tunnel 반환.
```

**데이터 경로**

```
handleConn(t, client):
  defer client.Close()
  backend, err := dialBackend(t)
  if err != nil { t.recordError(err); return }
  defer backend.Close()
  atomic.AddInt64(&t.activeConns, 1); defer atomic.AddInt64(&t.activeConns, -1)

  // 양방향 복사. 어느 쪽이든 끝나면 반대편 쓰기를 닫아 EOF를 전파한다.
  done := make(chan struct{}, 2)
  go func(){ n,_ := io.Copy(backend, client); atomic.AddInt64(&t.bytesUp, n)
             halfCloseWrite(backend); done <- struct{}{} }()
  go func(){ n,_ := io.Copy(client, backend); atomic.AddInt64(&t.bytesDown, n)
             halfCloseWrite(client);  done <- struct{}{} }()
  <-done; <-done

dialBackend(t):
  case "direct":
      ip := t.cachedIP
      if ip is zero { ip = ResolveGuestIP(t.VMName); t.cachedIP = ip }
      conn, err := net.DialTimeout("tcp", ip:guestPort, 10s)
      if err != nil {
          // IP가 바뀌었을 가능성 → 1회 재해석 후 재시도.
          // 이것이 VM 재기동·체크포인트 롤백 대응의 핵심이다.
          t.cachedIP = zero
          ip = ResolveGuestIP(t.VMName); t.cachedIP = ip
          conn, err = net.DialTimeout("tcp", ip:guestPort, 10s)
      }
      return conn, err
  case "ssh":
      cli := sshPool.Get(t.VMName)          // 죽어 있으면 내부에서 재연결
      // 게스트 sshd 가 게스트 로컬 스택으로 연결을 만들어 준다.
      // 그래서 게스트 127.0.0.1 에만 바인딩된 서비스에 도달할 수 있다.
      return cli.Dial("tcp", "127.0.0.1:"+guestPort)
```

`direct` 모드의 캐시된 IP는 성능 최적화이자 "해석은 한 번, 재해석은 실패했을 때"라는 정책이다. IP를 영구 캐시하면 VM 재기동 후 죽은 주소로 계속 다이얼하고, 매번 해석하면 커넥션마다 PowerShell 프로세스가 뜬다. 실패 시 1회 재해석이 두 극단 사이의 옳은 지점이다.

**터널 닫기**

```
Close(id):
  1. 리스너 전부 닫기 → accept 루프 종료
  2. 진행 중인 커넥션을 강제로 끊는다(트래킹된 net.Conn 목록을 Close).
     graceful drain 을 하지 않는 이유: 터널을 닫으라는 지시는 즉시성을 기대하며,
     장수명 커넥션(WebSocket, DB)이 있으면 drain 은 사실상 무기한 대기가 된다.
  3. 생성했던 방화벽 규칙을 이름으로 정확히 지정해 제거
  4. tailscale serve 를 붙였다면 해제
  5. tunnels.json 갱신
  ※ SSH 클라이언트는 풀 소유이므로 닫지 않는다. 다른 터널이 공유 중일 수 있다.
```

#### 4.6.3 Tailscale 연동

```
TailscaleIPs() ([]netip.Addr, error)
  1. tailscale.exe 경로: config → %ProgramFiles%\Tailscale\tailscale.exe → PATH
     없으면 TAILSCALE_UNAVAILABLE
  2. `tailscale.exe status --json` 실행 (5초 타임아웃)
  3. BackendState:
       "Running"                       → 진행
       "NeedsLogin" / "Stopped" / 기타 → TAILSCALE_NOT_RUNNING (상태 문자열을 detail 에)
  4. Self.TailscaleIPs 반환 (IPv4 100.64.0.0/10, IPv6 fd7a:115c:a1e0::/48)
```

`tailscale status --json`을 쓰는 이유는, 네트워크 인터페이스를 열거해 `100.64.0.0/10`을 찾는 방식이 CGNAT 대역을 쓰는 다른 소프트웨어와 충돌할 수 있고 백엔드 상태(로그인 여부)를 알려주지 못하기 때문이다.

| 방식 | 접근 주소 | TLS | 용도 |
|------|-----------|-----|------|
| `bind_scope: "tailnet"` | `100.x.y.z:<port>` | 없음(평문 TCP) | 임의의 TCP 서비스(DB, gRPC, SSH 등) |
| `tailscale_serve` | `https://<host>.<tailnet>.ts.net/` | Tailscale이 발급/갱신 | HTTP(S) 서비스 |

`tailscale serve`는 터널 위에 얹는 부가 계층이며 데이터 경로는 여전히 터널을 지난다.

```
tailscale serve --bg --https=<https_port> --set-path=<path> http://127.0.0.1:<tunnel_host_port>
```

`serve`의 백엔드로 `127.0.0.1:<host_port>`를 주므로 이 조합에서 터널의 `bind_scope`는 `loopback`이어야 한다. `tailnet` 스코프와 `serve`를 동시에 쓰면 같은 서비스가 평문 포트와 HTTPS 두 경로로 노출된다. 도구는 이 조합을 거부하지 않지만 `warnings`에 명시한다.

#### 4.6.4 자격증명 등록 흐름

`hypervm-mcp.exe cred set --vm <name> --user <user>`는 **비권한 사용자 세션에서** 실행된다. 하지만 자격증명을 복호화해야 하는 주체는 LocalSystem 서비스다. 두 컨텍스트가 다르므로 CLI가 직접 파일에 쓸 수 없다(사용자 스코프 DPAPI로 암호화하면 서비스가 못 읽고, 파일 ACL상 사용자는 쓰기 권한이 없다).

```
1. CLI가 콘솔에서 암호를 입력받는다 (에코 없음, term.ReadPassword)
   --ssh-key <path> 지정 시 개인키 파일을 읽는다(사용자 권한으로 읽으므로 CLI 측에서).
2. CLI가 파이프에 연결하여 제어 프레임을 전송한다.
     {"op":"cred.set","vm":"Dev-Ubuntu","username":"dev","password":"...",
      "ssh_port":22,"ssh_private_key":"..."}
3. 서비스가 credentials.dat 복호화 → 엔트리 병합 → 재암호화 → 원자적 교체
4. 서비스가 {"ok":true} 응답, CLI 종료
5. CLI 프로세스 메모리의 비밀 버퍼를 zero fill
```

**제어 프레임과 MCP 프레임의 구분.** 파이프 연결 직후 첫 1줄을 읽어 판별한다. `{"op":"..."}` 형태면 제어 핸들러로, `{"jsonrpc":"2.0",...}` 형태면 MCP 서버로 라우팅한다. 판별에 사용한 1줄은 MCP 경로로 갈 경우 `io.MultiReader`로 스트림 앞에 되돌려 붙인다. 이렇게 하면 브리지와 CLI가 같은 파이프 이름을 공유할 수 있다.

제어 op 목록: `cred.set`, `cred.list`(사용자명·SSH 포트·키 보유 여부만 반환), `cred.delete`, `tunnel.list`, `ping`, `status`.

#### 4.6.5 동시성

- PowerShell Runner는 호출마다 독립 프로세스를 띄우므로 상태를 공유하지 않는다. 세마포어로 동시 실행 수만 제한한다.
- Credential Store와 known_hosts는 `sync.RWMutex`로 보호하고, 쓰기는 항상 원자적 교체를 사용한다.
- Tunnel Manager는 터널 맵을 `sync.RWMutex`로 보호한다. 개별 터널의 카운터는 `atomic`으로 갱신하여 데이터 경로에서 락을 잡지 않는다.
- SSH Client Pool은 VM별 `singleflight` 패턴으로 중복 연결 수립을 방지한다.

## 5. API Design

### 5-1. New / Modified

#### 5-1-1. CLI 명령

```
hypervm-mcp.exe bridge
    stdio <-> named pipe 브리지. MCP 클라이언트가 실행하는 진입점.

hypervm-mcp.exe service install [--allowed-sid <sid>]
    바이너리를 ProgramData로 복사하고 서비스를 등록·시작한다.
    비상승 상태면 UAC로 자기 자신을 재실행한다. 반복 실행 시 설정이 수렴한다.

hypervm-mcp.exe service uninstall [--purge]
    서비스를 중지·삭제하고 열려 있던 방화벽 규칙을 정리한다.
    --purge 지정 시 ProgramData 디렉터리(자격증명 포함)도 제거한다.

hypervm-mcp.exe service start | stop | status
hypervm-mcp.exe service run                      # SCM 전용 내부 진입점

hypervm-mcp.exe cred set --vm <name> --user <user> [--ssh-key <path>] [--ssh-port <n>]
hypervm-mcp.exe cred list                        # 비밀은 출력하지 않는다
hypervm-mcp.exe cred delete --vm <name>

hypervm-mcp.exe tunnel list                      # MCP 없이 현재 터널 확인

hypervm-mcp.exe doctor
    진단: 서비스 상태, 파이프 접근 가능성, Hyper-V 기능 활성화, PowerShell Hyper-V 모듈,
    현재 사용자 SID와 allowed_sid 일치, 호스트 기본 VM/VHD 경로의 LocalSystem 접근성,
    가상 스위치 목록과 유형, Tailscale 설치·로그인 상태와 할당 IP,
    활성 터널 및 방화벽 규칙 정합성을 점검한다.
```

#### 5-1-2. MCP 도구 — VM 라이프사이클

```go
// ListVMs returns all virtual machines registered on the local Hyper-V host.
// The name filter accepts wildcard patterns (e.g. "Dev-*"); when empty, all VMs
// are returned. Results are sorted by name for stable output across calls.
type ListVMsInput struct {
    Name string `json:"name,omitempty" jsonschema:"Optional wildcard filter on VM name"`
}
type VMSummary struct {
    Name           string `json:"name"`
    ID             string `json:"id"`              // VM GUID
    State          string `json:"state"`           // Running | Off | Paused | Saved | Starting | Stopping
    CPUUsage       int    `json:"cpu_usage"`       // percent
    MemoryAssigned int64  `json:"memory_assigned"` // bytes
    UptimeSeconds  int64  `json:"uptime_seconds"`
    Generation     int    `json:"generation"`      // 1 or 2
}
func ListVMs(ctx context.Context, in ListVMsInput) ([]VMSummary, error)
// pseudocode:
//   script := `if ($P.name) { Get-VM -Name $P.name -ErrorAction SilentlyContinue }
//              else         { Get-VM } |
//              Select-Object Name, Id, State, CPUUsage, MemoryAssigned, Uptime, Generation |
//              Sort-Object Name`
//   // Uptime은 TimeSpan 객체로 직렬화되므로 TotalSeconds 필드를 추출한다.

// GetVM returns detailed information for a single VM: state, hardware, all
// attached disks with their on-disk paths, network adapters with switch names
// and reported guest IPs, checkpoint count, and the configured storage paths
// (config, checkpoint, smart paging).
func GetVM(ctx context.Context, in struct{ Name string }) (*VMDetail, error)

// StartVM powers on a VM. Already-Running is a no-op that succeeds. A Saved VM
// is resumed. This does NOT wait for the guest OS to finish booting; use
// WaitForGuestIP for that.
func StartVM(ctx context.Context, in struct{ Name string }) (*VMSummary, error)

// StopVM shuts down a VM. When force is false, an ACPI shutdown request is sent
// to the guest OS and the call waits up to timeout_seconds for the Off state.
// When force is true, the VM is powered off immediately, which may cause guest
// filesystem corruption.
type StopVMInput struct {
    Name           string `json:"name"`
    Force          bool   `json:"force,omitempty"`
    TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default 120, ignored when force
}
func StopVM(ctx context.Context, in StopVMInput) (*VMSummary, error)
// pseudocode:
//   if force: `Stop-VM -Name $P.name -TurnOff -Force`
//   else:     `Stop-VM -Name $P.name -Force
//              # -Force here suppresses the confirmation prompt, NOT a hard
//              # power-off. Graceful ACPI shutdown is Stop-VM's default.
//              $deadline = (Get-Date).AddSeconds($P.timeout)
//              while ((Get-VM -Name $P.name).State -ne 'Off') {
//                if ((Get-Date) -gt $deadline) { throw "shutdown timed out" }
//                Start-Sleep -Milliseconds 500
//              }`

func RestartVM(ctx context.Context, in StopVMInput) (*VMSummary, error)

// SuspendVM saves VM state to disk (mode "save") or pauses it in memory
// (mode "pause"). ResumeVM returns it to Running.
func SuspendVM(ctx context.Context, in struct{ Name, Mode string }) (*VMSummary, error)
func ResumeVM(ctx context.Context, in struct{ Name string }) (*VMSummary, error)

// WaitForGuestIP polls the VM's integration services until it reports a usable
// (non-link-local, non-loopback) IP address, or the timeout elapses. This is the
// correct way to wait for a VM to become reachable after StartVM: the Running
// state is reported long before the guest network stack is up.
//
// When the VM has adapters on multiple switches (for example a Default Switch
// and an External switch), all reported addresses are returned so the caller can
// pick the one on the network it cares about.
type WaitForGuestIPInput struct {
    Name           string `json:"name"`
    TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default 120
    Subnet         string `json:"subnet,omitempty"`          // e.g. "192.168.0.0/24"; wait for an address in this range
}
type GuestIPResult struct {
    Address       string   `json:"address"`
    AllAddresses  []string `json:"all_addresses"`
    WaitedSeconds float64  `json:"waited_seconds"`
}
func WaitForGuestIP(ctx context.Context, in WaitForGuestIPInput) (*GuestIPResult, error)
```

#### 5-1-3. MCP 도구 — 체크포인트

```go
// CreateCheckpoint takes a snapshot of the VM. If snapshot_name is empty,
// Hyper-V generates a timestamped name. Works on Running and Off VMs; for
// Running VMs the checkpoint type follows the VM's CheckpointType setting.
// Checkpoint files are written to the VM's SnapshotFileLocation, which can be
// set at creation time or with SetVMStoragePaths.
func CreateCheckpoint(ctx context.Context, in struct{ VMName, SnapshotName string }) (*Checkpoint, error)

// ListCheckpoints returns all checkpoints ordered oldest to newest, with
// parent-child relationships expressed via ParentSnapshotName.
func ListCheckpoints(ctx context.Context, in struct{ VMName string }) ([]Checkpoint, error)

// ApplyCheckpoint reverts the VM to the given checkpoint. This DISCARDS all state
// changes made after the checkpoint was taken. A Running VM must be stopped
// first; this function stops it when auto_stop is true, otherwise it errors.
//
// Tunnels targeting this VM survive the revert: they re-resolve the guest IP on
// their next backend dial. SSH host keys may change if the checkpoint predates
// key generation, in which case SSHExec reports SSH_HOST_KEY_MISMATCH.
func ApplyCheckpoint(ctx context.Context, in struct {
    VMName, SnapshotName string; AutoStop bool
}) (*VMSummary, error)

// DeleteCheckpoint removes a checkpoint and merges its differencing disk into
// the parent. When include_children is true, the entire subtree is removed.
// Merging is asynchronous in Hyper-V; this call returns once the merge completes.
func DeleteCheckpoint(ctx context.Context, in struct {
    VMName, SnapshotName string; IncludeChildren bool
}) error
```

#### 5-1-4. MCP 도구 — VM 생성/삭제, 하드웨어, 저장 위치

```go
// CreateVM provisions a new virtual machine.
//
// STORAGE PLACEMENT. All four path families can be set independently:
//   vm_path            — directory for the VM configuration files. IMMUTABLE
//                        after creation; the only way to move a VM's config
//                        later is ExportVM followed by ImportVM. Defaults to the
//                        host's VirtualMachinePath.
//   vhd_path           — full path of the VHDX file. When empty, a new dynamic
//                        VHDX of vhd_size_gb is created under the host's
//                        VirtualHardDiskPath. When it points to an existing file
//                        that file is attached as-is and vhd_size_gb is ignored.
//   checkpoint_path    — directory for .avhdx checkpoint files. Mutable later.
//   smart_paging_path  — directory for the smart paging file. Mutable later.
//
// Every path is validated for LocalSystem accessibility before any Hyper-V
// object is created, so a bad path fails cleanly instead of leaving a
// half-provisioned VM. Mapped drive letters (Z:\) are rejected outright: they do
// not exist in the service's logon session. Use UNC paths instead, and grant the
// machine account access to the share.
//
// Generation 2 is the default and is required for UEFI/Secure Boot guests;
// Generation 1 must be chosen explicitly for legacy BIOS guests.
type CreateVMInput struct {
    Name            string `json:"name"`
    Generation      int    `json:"generation,omitempty"`      // default 2
    MemoryMB        int    `json:"memory_mb,omitempty"`       // default 4096
    DynamicMemory   bool   `json:"dynamic_memory,omitempty"`  // default true
    CPUCount        int    `json:"cpu_count,omitempty"`       // default 2
    VMPath          string `json:"vm_path,omitempty"`
    VHDPath         string `json:"vhd_path,omitempty"`
    VHDSizeGB       int    `json:"vhd_size_gb,omitempty"`     // default 64
    CheckpointPath  string `json:"checkpoint_path,omitempty"`
    SmartPagingPath string `json:"smart_paging_path,omitempty"`
    SwitchName      string `json:"switch_name,omitempty"`     // when empty, no NIC is connected
    StaticMAC       string `json:"static_mac,omitempty"`      // enables router DHCP reservation
    VLANID          int    `json:"vlan_id,omitempty"`
    ISOPath         string `json:"iso_path,omitempty"`        // attaches a DVD drive, sets boot order
    CreateParents   bool   `json:"create_parents,omitempty"`  // mkdir -p for the directories above
}
func CreateVM(ctx context.Context, in CreateVMInput) (*VMDetail, error)
// pseudocode:
//   1. Get-VM -Name $P.name 이 성공하면 VM_ALREADY_EXISTS (New-VM은 중복 이름을 허용한다)
//   2. ValidatePath(vm_path, create), (vhd_path, create), (checkpoint_path, create),
//      (smart_paging_path, create), (iso_path, read)   — 전부 통과해야 진행
//   3. New-VM -Name -MemoryStartupBytes -Generation [-Path <vm_path>]
//              [-NewVHDPath -NewVHDSizeBytes | -VHDPath] [-SwitchName]
//   4. Set-VMProcessor -Count
//   5. if dynamic_memory: Set-VMMemory -DynamicMemoryEnabled $true
//   6. if checkpoint_path:   Set-VM -SnapshotFileLocation
//      if smart_paging_path: Set-VM -SmartPagingFilePath
//   7. if static_mac: Set-VMNetworkAdapter -StaticMacAddress
//      if vlan_id:    Set-VMNetworkAdapterVlan -Access -VlanId
//   8. if iso_path: Add-VMDvdDrive -Path; (Gen2) Set-VMFirmware -FirstBootDevice <dvd>
//   9. Get-VM 상세 직렬화 반환

// CreateVMFromTemplate provisions a VM in seconds by creating a differencing
// disk whose parent is an existing golden VHDX, instead of installing an OS.
//
// The parent image must not be modified while children exist — doing so corrupts
// every child. This function warns when the parent is writable but does not
// refuse, since sharing one parent across many VMs is the intended workflow.
//
// The new VM inherits the parent image's machine identity: hostname, Windows SID,
// SSH host keys, and machine-id. Running several clones concurrently causes name
// collisions on the network. The result's warnings field says so; re-generalizing
// the guest (sysprep on Windows, ssh-keygen -A and a new machine-id on Linux) is
// left to the caller.
type CreateVMFromTemplateInput struct {
    Name            string `json:"name"`
    ParentVHDPath   string `json:"parent_vhd_path"`
    VMPath          string `json:"vm_path,omitempty"`
    VHDPath         string `json:"vhd_path,omitempty"`   // the differencing disk; defaults next to parent
    Generation      int    `json:"generation,omitempty"` // must match the parent image; default 2
    MemoryMB        int    `json:"memory_mb,omitempty"`
    CPUCount        int    `json:"cpu_count,omitempty"`
    SwitchName      string `json:"switch_name,omitempty"`
    StaticMAC       string `json:"static_mac,omitempty"`
    CreateParents   bool   `json:"create_parents,omitempty"`
}
func CreateVMFromTemplate(ctx context.Context, in CreateVMFromTemplateInput) (*VMDetail, error)

// DeleteVM removes a VM. When delete_disks is true, VHD/VHDX files attached to
// the VM are deleted after removal; disks shared with other VMs (including
// parents of differencing disks) are never deleted, and the result lists what was
// kept and why. The VM must be Off; when force is true it is powered off first.
// Tunnels targeting this VM are closed automatically and reported.
type DeleteVMResult struct {
    Deleted        bool     `json:"deleted"`
    DisksDeleted   []string `json:"disks_deleted"`
    DisksKept      []string `json:"disks_kept"`
    KeptReasons    []string `json:"kept_reasons"`
    TunnelsClosed  []string `json:"tunnels_closed"`
}
func DeleteVM(ctx context.Context, in struct {
    Name string; DeleteDisks, Force bool
}) (*DeleteVMResult, error)

// SetVMHardware adjusts CPU count and memory. Static memory changes require the
// VM to be Off; dynamic memory range changes apply while Running. Zero fields
// are left unchanged.
func SetVMHardware(ctx context.Context, in struct {
    Name string; CPUCount, MemoryMB int; DynamicMemory *bool
}) (*VMDetail, error)

// SetVMStoragePaths changes the checkpoint and smart paging directories of an
// existing VM. The VM configuration path CANNOT be changed here — it is fixed at
// creation. Changing the checkpoint location fails when checkpoints already
// exist; delete them first or export and re-import the VM.
func SetVMStoragePaths(ctx context.Context, in struct {
    Name, CheckpointPath, SmartPagingPath string; CreateParents bool
}) (*VMDetail, error)

// GetHostStoragePaths returns the host's default VirtualMachinePath and
// VirtualHardDiskPath, plus a LocalSystem accessibility verdict for each. Call
// this before creating VMs without explicit paths, so you know where files will
// actually land.
type HostStoragePaths struct {
    VirtualMachinePath   string `json:"virtual_machine_path"`
    VirtualHardDiskPath  string `json:"virtual_hard_disk_path"`
    VMPathAccessible     bool   `json:"vm_path_accessible"`
    VHDPathAccessible    bool   `json:"vhd_path_accessible"`
    ImageLibraryPath     string `json:"image_library_path,omitempty"`
    FreeSpaceBytes       map[string]int64 `json:"free_space_bytes"` // by drive root
}
func GetHostStoragePaths(ctx context.Context, in struct{}) (*HostStoragePaths, error)

// SetHostStoragePaths changes the host-wide defaults. Existing VMs are not
// moved; only subsequently created VMs are affected.
func SetHostStoragePaths(ctx context.Context, in struct {
    VirtualMachinePath, VirtualHardDiskPath string; CreateParents bool
}) (*HostStoragePaths, error)

// CreateVHD creates a virtual hard disk file without attaching it.
// disk_type must be "dynamic" (default), "fixed", or "differencing".
// For "differencing", parent_path is required and size_gb is ignored.
func CreateVHD(ctx context.Context, in struct {
    Path string; SizeGB int; DiskType, ParentPath string; CreateParents bool
}) (*VHDInfo, error)

// AttachVHD attaches an existing VHD/VHDX to a VM's SCSI controller.
// DetachVHD removes it without deleting the file.
func AttachVHD(ctx context.Context, in struct{ VMName, Path string }) (*VMDetail, error)
func DetachVHD(ctx context.Context, in struct{ VMName, Path string }) (*VMDetail, error)

// GetVHDInfo reports a disk's type, size, parent chain, and fragmentation.
func GetVHDInfo(ctx context.Context, in struct{ Path string }) (*VHDInfo, error)

// ResizeVHD grows (or, for unpartitioned space, shrinks) a VHDX. The VM must be
// Off. Growing the file does NOT grow the guest filesystem; that is a guest-side
// operation left to the caller.
func ResizeVHD(ctx context.Context, in struct{ Path string; SizeGB int }) (*VHDInfo, error)

// ConvertVHD converts between VHD and VHDX and between dynamic and fixed,
// writing to destination_path. The source is left untouched.
func ConvertVHD(ctx context.Context, in struct {
    SourcePath, DestinationPath, Format, DiskType string; CreateParents bool
}) (*VHDInfo, error)

// ExportVM writes a self-contained copy of a VM (configuration, all disks, and
// optionally checkpoints) into export_path/<vm name>/. This is the canonical way
// to produce a golden image or to relocate a VM's immutable configuration path.
// The VM may be Running; Hyper-V exports a consistent point-in-time copy.
func ExportVM(ctx context.Context, in struct {
    Name, ExportPath string; CreateParents bool
}) (*ExportResult, error)

// ImportVM registers a VM from a previously exported directory.
//   copy=false  — register in place, using the files where they are
//   copy=true   — copy the files to vm_path/vhd_path first, then register
// generate_new_id assigns a new VM GUID, which is required when importing a
// second copy of a VM that is already registered.
func ImportVM(ctx context.Context, in struct {
    ExportedPath, NewName, VMPath, VHDPath string
    Copy, GenerateNewID, CreateParents bool
}) (*VMDetail, error)
```

#### 5-1-5. MCP 도구 — 네트워크 토폴로지

```go
// ListPhysicalAdapters returns the host's physical network adapters with link
// state, speed, whether the adapter is wireless, and whether it is already bound
// to a Hyper-V external switch. Use this to choose a NIC for CreateSwitch.
type PhysicalAdapter struct {
    Name            string `json:"name"`
    InterfaceDesc   string `json:"interface_description"`
    Status          string `json:"status"`          // Up | Disconnected | Disabled
    LinkSpeedMbps   int64  `json:"link_speed_mbps"`
    MACAddress      string `json:"mac_address"`
    IsWireless      bool   `json:"is_wireless"`
    BoundToSwitch   string `json:"bound_to_switch,omitempty"`
    IPAddresses     []string `json:"ip_addresses"`
}
func ListPhysicalAdapters(ctx context.Context, in struct{}) ([]PhysicalAdapter, error)

// ListSwitches returns all virtual switches with their type and, for External
// switches, the bound physical adapter and whether the management OS shares it.
type VMSwitch struct {
    Name              string   `json:"name"`
    SwitchType        string   `json:"switch_type"`   // External | Internal | Private
    NetAdapterName    string   `json:"net_adapter_name,omitempty"`
    AllowManagementOS bool     `json:"allow_management_os"`
    ConnectedVMs      []string `json:"connected_vms"`
}
func ListSwitches(ctx context.Context, in struct{}) ([]VMSwitch, error)

// CreateSwitch creates a virtual switch.
//
// switch_type "External" BRIEFLY DISCONNECTS THE HOST from the network while
// Hyper-V rebinds the chosen physical adapter. Any remote session to this host
// will drop. Because of that, this call requires confirm_disruption to be true
// for External switches.
//
// allow_management_os (default true) keeps the host reachable through the same
// adapter. Setting it to false on a machine with only one physical adapter would
// cut the host off entirely, so that combination is rejected.
//
// Creating an External switch is what makes a guest a first-class node on the
// physical LAN, with its own 192.168.0.x address. That is required — and port
// forwarding is not a substitute — when the guest must serve SMB, when the
// protocol is sensitive to host identity such as Kerberos, or when other LAN
// devices must reach the guest directly. See also DiagnoseVMNetwork.
type CreateSwitchInput struct {
    Name              string `json:"name"`
    SwitchType        string `json:"switch_type"`                    // External | Internal | Private
    NetAdapterName    string `json:"net_adapter_name,omitempty"`     // required for External
    AllowManagementOS *bool  `json:"allow_management_os,omitempty"`  // default true
    ConfirmDisruption bool   `json:"confirm_disruption,omitempty"`   // required for External
    Notes             string `json:"notes,omitempty"`
}
func CreateSwitch(ctx context.Context, in CreateSwitchInput) (*VMSwitch, error)

// DeleteSwitch removes a virtual switch. VMs connected to it lose network
// connectivity; the result lists them. For External switches this again briefly
// disrupts host networking as the adapter is unbound.
func DeleteSwitch(ctx context.Context, in struct {
    Name string; ConfirmDisruption bool
}) (*DeleteSwitchResult, error)

// SetVMNetwork configures a VM's network adapter: which switch it is on, its MAC
// address, VLAN membership, and MAC spoofing.
//
// A static MAC is the least invasive way to give a guest a stable address: pair
// it with a DHCP reservation on the LAN router and the guest keeps its IP across
// reinstalls without any guest-side configuration.
//
// mac_spoofing must be enabled when the guest itself runs nested VMs or
// containers with bridged networking, because their frames carry MACs the switch
// has not learned for that port and are dropped otherwise.
type SetVMNetworkInput struct {
    VMName        string `json:"vm_name"`
    AdapterName   string `json:"adapter_name,omitempty"` // default: the VM's first adapter
    SwitchName    string `json:"switch_name,omitempty"`  // "" leaves it unchanged; "-" disconnects
    StaticMAC     string `json:"static_mac,omitempty"`   // "dynamic" reverts to a generated MAC
    VLANID        *int   `json:"vlan_id,omitempty"`      // 0 clears VLAN tagging
    MACSpoofing   *bool  `json:"mac_spoofing,omitempty"`
    CreateAdapter bool   `json:"create_adapter,omitempty"` // add an adapter if the VM has none
}
func SetVMNetwork(ctx context.Context, in SetVMNetworkInput) (*VMDetail, error)

// SetGuestStaticIP configures a static address inside the guest OS.
//
// Windows guests are configured over PowerShell Direct, which needs no guest
// network and is therefore safe. Linux guests must be configured over SSH, which
// means the command cuts its own connection: the change is applied in the
// background and this function then polls the NEW address to confirm success. If
// the new address never answers, the guest is likely stranded — which is why
// auto_checkpoint defaults to true and takes a checkpoint before touching
// anything.
//
// Supported Linux paths are NetworkManager (nmcli) and netplan. When neither is
// detected, this returns UNSUPPORTED_GUEST_OS along with the exact commands to
// run manually, rather than guessing at the distribution's network stack.
type SetGuestStaticIPInput struct {
    VMName         string   `json:"vm_name"`
    Address        string   `json:"address"`         // "192.168.0.42"
    PrefixLength   int      `json:"prefix_length"`   // 24
    Gateway        string   `json:"gateway,omitempty"`
    DNSServers     []string `json:"dns_servers,omitempty"`
    InterfaceName  string   `json:"interface_name,omitempty"` // default: the primary adapter
    AutoCheckpoint bool     `json:"auto_checkpoint,omitempty"` // default true
    TimeoutSeconds int      `json:"timeout_seconds,omitempty"` // default 120
}
type SetGuestStaticIPResult struct {
    Applied          bool     `json:"applied"`
    VerifiedAddress  string   `json:"verified_address,omitempty"`
    CheckpointName   string   `json:"checkpoint_name,omitempty"`
    Method           string   `json:"method"`            // "powershell-direct" | "nmcli" | "netplan"
    ManualCommands   []string `json:"manual_commands,omitempty"` // set on UNSUPPORTED_GUEST_OS
}
func SetGuestStaticIP(ctx context.Context, in SetGuestStaticIPInput) (*SetGuestStaticIPResult, error)

// DiagnoseVMNetwork reports a VM's network topology and what that topology can
// and cannot do, so the caller can decide between opening a tunnel and moving the
// VM to an External switch.
//
// It reports, per adapter: the switch and its type, the guest addresses reported
// by integration services, whether the host can reach the guest (TCP probe), and
// whether the guest is a first-class LAN node. It also lists which host ports are
// already occupied among the well-known ones that cannot be tunnelled (445, 3389,
// 5985, 139, 137), so a request to expose the guest's SMB service gets an
// accurate answer instead of a failed tunnel.
type NetworkDiagnosis struct {
    VMName            string   `json:"vm_name"`
    Adapters          []AdapterDiagnosis `json:"adapters"`
    HostCanReachGuest bool     `json:"host_can_reach_guest"`
    GuestOnPhysicalLAN bool    `json:"guest_on_physical_lan"`
    BlockedHostPorts  []int    `json:"blocked_host_ports"`  // occupied on the host; not tunnellable
    Recommendation    string   `json:"recommendation"`      // human-readable summary
}
func DiagnoseVMNetwork(ctx context.Context, in struct {
    VMName string; ProbePorts []int
}) (*NetworkDiagnosis, error)
```

#### 5-1-6. MCP 도구 — 게스트 실행/파일 전송 (PowerShell Direct)

```go
// GuestInvokeCommand runs a PowerShell command inside the guest OS over
// PowerShell Direct. The VM must be Running with Integration Services available.
// This path requires NO guest network at all, which makes it the right tool for
// bootstrapping a VM before it has an IP — configuring sshd, or setting a static
// address that would cut an SSH session.
//
// Credentials resolve as: (1) explicit username/password in this call, (2) the
// stored credential for this VM name. If neither is available the call fails
// rather than guessing.
type GuestInvokeInput struct {
    VMName         string `json:"vm_name"`
    Command        string `json:"command"`
    Username       string `json:"username,omitempty"`
    Password       string `json:"password,omitempty"`
    TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default 120
}
type GuestResult struct {
    Stdout   string `json:"stdout"`
    Stderr   string `json:"stderr"`
    ExitCode int    `json:"exit_code"`
}
func GuestInvokeCommand(ctx context.Context, in GuestInvokeInput) (*GuestResult, error)
// pseudocode:
//   1. Get-VM 상태가 Running 이 아니면 VM_WRONG_STATE
//   2. 자격증명 해석 (위 순서)
//   3. 스크립트 (비밀은 전부 $P 인자 채널로만 전달):
//        $sec  = ConvertTo-SecureString $P.password -AsPlainText -Force
//        $cred = New-Object PSCredential($P.username, $sec)
//        Invoke-Command -VMName $P.vm -Credential $cred `
//          -ScriptBlock ([ScriptBlock]::Create($P.command))
//   4. 로깅: 스크립트 본문과 자격증명은 기록하지 않는다. VM 이름과 소요 시간만 남긴다.
//
// [ScriptBlock]::Create($P.command) 는 게스트에서 임의 코드를 실행한다. 이는 도구의
// 의도된 기능이지 취약점이 아니다 — 호출자는 이미 파이프 DACL을 통과한 신뢰 주체다.

// GuestCopyFile copies a file from the host into the guest using Copy-VMFile,
// which requires the Guest Service Interface integration component; this function
// enables it automatically when disabled. Copy-VMFile itself is host-to-guest
// only. GuestCopyFrom handles the reverse for Windows guests with a PowerShell
// Direct session and Copy-Item -FromSession.
//
// The source path is read by the service as LocalSystem, so the LocalSystem path
// rules apply: no mapped drive letters.
func GuestCopyFile(ctx context.Context, in struct {
    VMName, SourcePath, DestinationPath string
    CreateFullPath, Overwrite bool
}) error

// GetGuestNetwork returns the IP addresses reported by integration services for
// each adapter, with the switch each adapter is on. Returns an empty address list
// rather than an error when integration services have not yet reported (common
// shortly after boot).
func GetGuestNetwork(ctx context.Context, in struct{ VMName string }) ([]VMNetworkAdapter, error)
```

#### 5-1-7. MCP 도구 — SSH

```go
// SSHExec runs a command in the guest over SSH and returns its output. Unlike
// GuestInvokeCommand this requires guest networking and a running sshd, but it
// works with any OS — Linux guests in particular, where PowerShell Direct does
// not exist — and does not depend on Hyper-V integration services.
//
// Credentials resolve as: (1) explicit fields in this call, (2) the stored
// credential for this VM name. Public key auth is attempted before password auth
// when a key is available.
//
// The host key is pinned per VM name on first connect (TOFU). A later mismatch
// fails with SSH_HOST_KEY_MISMATCH unless trust_new_key is set — the expected
// path after recreating a VM or reverting to an older checkpoint.
type SSHExecInput struct {
    VMName         string `json:"vm_name"`
    Command        string `json:"command"`
    Username       string `json:"username,omitempty"`
    Password       string `json:"password,omitempty"`
    PrivateKey     string `json:"private_key,omitempty"` // PEM; overrides stored key
    Host           string `json:"host,omitempty"`        // override the resolved guest IP
    Port           int    `json:"port,omitempty"`        // default: stored ssh_port, else 22
    TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default 120
    TrustNewKey    bool   `json:"trust_new_key,omitempty"`
    Stdin          string `json:"stdin,omitempty"`
}
type SSHExecResult struct {
    Stdout             string `json:"stdout"`
    Stderr             string `json:"stderr"`
    ExitCode           int    `json:"exit_code"`
    HostKeyFingerprint string `json:"host_key_fingerprint"`
    HostKeyFirstSeen   bool   `json:"host_key_first_seen"`
}
func SSHExec(ctx context.Context, in SSHExecInput) (*SSHExecResult, error)

// SSHInfo reports everything needed to reach the VM over SSH from an external
// tool, without executing anything: resolved address, port, stored username,
// available auth methods, and a ready-to-paste ssh command line. reachable_from
// tells the caller which vantage points can use that address directly — the host
// always, the LAN only when the VM is on an External switch, the tailnet only
// through an open tunnel.
type SSHInfoResult struct {
    VMName        string   `json:"vm_name"`
    Address       string   `json:"address"`
    AllAddresses  []string `json:"all_addresses"`
    Port          int      `json:"port"`
    Username      string   `json:"username"`
    AuthMethods   []string `json:"auth_methods"`   // "publickey", "password"
    HostKeyPinned bool     `json:"host_key_pinned"`
    Fingerprint   string   `json:"fingerprint,omitempty"`
    SSHCommand    string   `json:"ssh_command"`
    ReachableFrom []string `json:"reachable_from"` // "host", "lan", "tailnet"
}
func SSHInfo(ctx context.Context, in struct{ VMName string }) (*SSHInfoResult, error)

// SSHForgetHostKey removes the pinned host key for a VM. Use after recreating a
// VM when you prefer an explicit reset over passing trust_new_key.
func SSHForgetHostKey(ctx context.Context, in struct{ VMName string }) error
```

#### 5-1-8. MCP 도구 — 터널

```go
// OpenTunnel creates a persistent TCP forward from the host to a port inside a
// VM. The tunnel lives in the service process, so it outlives this MCP session
// and is restored when the service restarts.
//
// mode selects the data path:
//   "direct" — the service dials the VM's IP directly. Requires the guest service
//              to be bound to an address reachable from the host (0.0.0.0 or the
//              guest's LAN IP). Lowest overhead; no guest sshd required.
//   "ssh"    — the service opens an SSH channel to the guest and forwards through
//              it. This is the ONLY way to reach a guest service bound to the
//              guest's own 127.0.0.1. Requires sshd and credentials.
//
// bind_scope selects who can reach the tunnel:
//   "loopback" — 127.0.0.1 only. This host. (default)
//   "tailnet"  — the host's Tailscale addresses, so any tailnet peer can connect.
//                Requires Tailscale installed and logged in. A Windows Firewall
//                inbound rule scoped to those addresses is created automatically
//                and removed on close.
//   "all"      — 0.0.0.0 and ::. Exposes the service to every network the host is
//                on, including untrusted LANs. Opt in deliberately.
//   or an explicit IP literal to bind exactly one address.
//
// NOT EVERY SERVICE CAN BE TUNNELLED. A tunnel needs a free host port, and the
// client must be able to address that port. SMB (445), RDP (3389), WinRM (5985)
// and NetBIOS (137-139) are occupied by the host itself, and SMB clients cannot
// even express a non-default port in a UNC path. For those, put the VM on an
// External switch instead so it gets its own LAN address — see CreateSwitch and
// DiagnoseVMNetwork.
type OpenTunnelInput struct {
    VMName      string `json:"vm_name"`
    GuestPort   int    `json:"guest_port"`
    HostPort    int    `json:"host_port,omitempty"`    // 0 = auto-assign
    Mode        string `json:"mode,omitempty"`         // "direct" (default) | "ssh"
    BindScope   string `json:"bind_scope,omitempty"`   // "loopback" (default) | "tailnet" | "all" | IP
    AutoRestore bool   `json:"auto_restore,omitempty"` // default true
    Label       string `json:"label,omitempty"`
}
type Tunnel struct {
    ID           string   `json:"id"`
    VMName       string   `json:"vm_name"`
    Mode         string   `json:"mode"`
    BindScope    string   `json:"bind_scope"`
    ListenAddrs  []string `json:"listen_addrs"`
    HostPort     int      `json:"host_port"`
    GuestPort    int      `json:"guest_port"`
    GuestAddress string   `json:"guest_address,omitempty"`
    URLs         []string `json:"urls"`
    ActiveConns  int64    `json:"active_conns"`
    TotalConns   int64    `json:"total_conns"`
    BytesUp      int64    `json:"bytes_up"`
    BytesDown    int64    `json:"bytes_down"`
    LastError    string   `json:"last_error,omitempty"`
    FirewallRule string   `json:"firewall_rule,omitempty"`
    ServeURL     string   `json:"serve_url,omitempty"`
    Warnings     []string `json:"warnings,omitempty"`
    Created      string   `json:"created"`
}
func OpenTunnel(ctx context.Context, in OpenTunnelInput) (*Tunnel, error)

// ListTunnels returns all tunnels held by the service, including ones opened by
// other MCP sessions or restored at service start. Filter by vm_name when set.
func ListTunnels(ctx context.Context, in struct{ VMName string }) ([]Tunnel, error)

// CloseTunnel stops a tunnel, drops its in-flight connections immediately,
// removes its firewall rule, detaches any tailscale serve mapping, and forgets
// its persisted definition.
func CloseTunnel(ctx context.Context, in struct{ ID string }) error

// TailscaleServe puts Tailscale's HTTPS front end in front of an existing
// loopback tunnel, making the VM service reachable at
// https://<host>.<tailnet>.ts.net<path> for tailnet peers, with a certificate
// Tailscale issues and renews. This is for HTTP services; for arbitrary TCP use
// bind_scope "tailnet" instead.
//
// This does NOT enable Tailscale Funnel, so the service is never exposed to the
// public internet by this tool.
type TailscaleServeInput struct {
    TunnelID  string `json:"tunnel_id"`
    Path      string `json:"path,omitempty"`       // default "/"
    HTTPSPort int    `json:"https_port,omitempty"` // default 443
    Off       bool   `json:"off,omitempty"`
}
func TailscaleServe(ctx context.Context, in TailscaleServeInput) (*Tunnel, error)

// TailnetStatus reports the host's Tailscale state: whether the CLI is present,
// the backend state, the host's tailnet addresses and MagicDNS name, and which
// tunnels are bound to tailnet addresses. Call this before opening a tunnel with
// bind_scope "tailnet" to give an accurate answer about what will be reachable.
type TailnetStatusResult struct {
    Installed      bool     `json:"installed"`
    BackendState   string   `json:"backend_state"` // "Running", "NeedsLogin", "Stopped", ...
    Addresses      []string `json:"addresses"`
    MagicDNSName   string   `json:"magic_dns_name,omitempty"`
    TailnetName    string   `json:"tailnet_name,omitempty"`
    ExposedTunnels []string `json:"exposed_tunnels"`
}
func TailnetStatus(ctx context.Context, in struct{}) (*TailnetStatusResult, error)
```

### 5-2. Error Handling

REST API가 아니므로 HTTP 상태 코드 대신 오류 코드를 정의한다. 모든 오류는 MCP 도구 결과의 `isError: true`와 함께 다음 구조로 직렬화된다.

```json
{ "code": "PORT_IN_USE", "message": "host port 445 is already in use",
  "detail": "445 is held by the host's LanmanServer and cannot be tunnelled. SMB clients cannot address a non-default port either. Put the VM on an External switch so it gets its own LAN address." }
```

| Code | 발생 조건 | 호출자가 취할 조치 |
|------|-----------|---------------------|
| `VM_NOT_FOUND` | 지정한 이름의 VM이 없음 | `list_vms`로 실제 이름 확인 |
| `VM_ALREADY_EXISTS` | `create_vm` 시 동명 VM 존재 | 다른 이름 사용 또는 기존 VM 삭제 |
| `VM_WRONG_STATE` | 현재 상태에서 불가능한 조작 | 상태 전이 후 재시도 |
| `CHECKPOINT_NOT_FOUND` | 지정한 스냅샷 없음 | `list_checkpoints`로 확인 |
| `CHECKPOINT_EXISTS` | 체크포인트가 있어 경로 변경 불가 | 체크포인트 삭제 후 재시도 |
| `SWITCH_NOT_FOUND` | 지정한 가상 스위치 없음 | `list_switches`로 확인 |
| `SWITCH_IN_USE` | 삭제하려는 스위치에 VM이 연결됨 | 연결 해제 후 재시도 (결과에 VM 목록 포함) |
| `ADAPTER_NOT_FOUND` | 지정한 물리 어댑터 없음 | `list_physical_adapters`로 확인 |
| `NETWORK_DISRUPTION_NOT_CONFIRMED` | External 스위치 조작에 `confirm_disruption` 미지정 | 호스트 네트워크 단절을 감수하고 `true` 지정 |
| `PATH_NOT_FOUND` | 경로 또는 상위 디렉터리 없음 | 경로 확인, 또는 `create_parents: true` |
| `PATH_NOT_ACCESSIBLE` | LocalSystem이 접근할 수 없는 경로 | 매핑 드라이브면 UNC로 변경, UNC면 공유 ACL에 머신 계정 추가 (4.5.3) |
| `PATH_IMMUTABLE` | VM 구성 파일 경로를 변경하려 함 | `export_vm` → `import_vm` 경유 |
| `INSUFFICIENT_SPACE` | 대상 볼륨 여유 공간 부족 | 다른 경로 지정 또는 공간 확보 (필요/가용 용량을 detail에 포함) |
| `VHD_IN_USE` | 다른 VM이 사용 중인 VHD를 삭제·변경하려 함 | 사용 중인 VM 목록을 확인 후 재시도 |
| `PARENT_VHD_NOT_FOUND` | 차등 디스크의 부모 이미지 없음 | `parent_vhd_path` 확인 |
| `CREDENTIAL_NOT_FOUND` | 게스트 자격증명 미등록 및 미지정 | `hypervm-mcp cred set` 실행 |
| `GUEST_AUTH_FAILED` | PowerShell Direct 인증 실패 | 자격증명 재등록 |
| `GUEST_SERVICE_UNAVAILABLE` | 통합 서비스 미동작 또는 부팅 미완료 | `wait_for_guest_ip` 후 재시도 |
| `GUEST_IP_UNAVAILABLE` | 통합 서비스가 사용 가능한 IP를 보고하지 않음 | `wait_for_guest_ip` 사용, NIC의 스위치 연결 확인 |
| `UNSUPPORTED_GUEST_OS` | 게스트 OS에 대해 자동 구성 경로가 없음 | 결과의 `manual_commands`를 수동 실행 |
| `SSH_AUTH_FAILED` | SSH 인증 실패 (키·암호 모두 거부) | 자격증명 재등록, 게스트 `authorized_keys` 확인 |
| `SSH_HOST_KEY_MISMATCH` | 고정된 호스트키와 불일치 | VM 재생성·롤백이 원인이면 `trust_new_key: true` 또는 `ssh_forget_host_key` |
| `SSH_UNREACHABLE` | TCP 연결 실패 또는 sshd 미동작 | VM 부팅 상태, sshd 서비스, 게스트 방화벽 확인 |
| `TUNNEL_NOT_FOUND` | 지정한 터널 ID 없음 | `list_tunnels`로 확인 |
| `PORT_IN_USE` | 호스트 포트가 이미 점유됨 | 다른 포트 또는 `host_port: 0`. 시스템 점유 포트면 External 스위치 사용 |
| `TAILSCALE_UNAVAILABLE` | tailscale.exe를 찾을 수 없음 | Tailscale 설치, 또는 `config.json`의 `tailscale_path` 지정 |
| `TAILSCALE_NOT_RUNNING` | 백엔드가 Running이 아님 | `tailscale up` 실행 후 재시도 |
| `FIREWALL_ERROR` | 방화벽 규칙 생성/삭제 실패 | 경고로만 처리되며 터널은 동작할 수 있음. 로그 확인 |
| `OPERATION_TIMEOUT` | PowerShell 또는 상태 전이 타임아웃 | 타임아웃 상향 또는 VM 상태 직접 확인 |
| `HYPERV_UNAVAILABLE` | Hyper-V 기능 비활성 또는 VMMS 미동작 | `hypervm-mcp doctor` 실행 |
| `POWERSHELL_ERROR` | 분류되지 않은 PowerShell 오류 | `detail`의 원문 메시지 참조 |
| `INVALID_ARGUMENT` | 필수 인자 누락 또는 값 범위 위반 | 인자 수정 |
| `INTERNAL` | 위 어디에도 속하지 않는 서버 오류 | 서비스 로그 확인 |

PowerShell 오류 메시지에서 위 코드로의 매핑은 `CategoryInfo.Category`와 `FullyQualifiedErrorId` 패턴 매칭으로 수행한다. 매칭되지 않으면 `POWERSHELL_ERROR`로 폴백하며 원문을 `detail`에 보존한다. **분류 실패가 정보 손실로 이어지지 않는다.**

브리지 프로세스 자체의 오류(MCP 이전 단계)는 stderr로만 보고한다.

| 상황 | stderr 메시지 |
|------|---------------|
| 파이프 없음 (`ERROR_FILE_NOT_FOUND`) | 서비스가 실행 중이 아닙니다. `hypervm-mcp service start`를 실행하세요. |
| 접근 거부 (`ERROR_ACCESS_DENIED`) | 현재 사용자가 허용된 SID와 일치하지 않습니다. `hypervm-mcp doctor`로 확인하세요. |
| 재시도 소진 | 서비스 응답 없음 (10초 타임아웃). 서비스 로그를 확인하세요. |

## 6. Implementation Plan

### 6-1. Milestones

각 Phase는 독립적으로 검증 가능하며, 이전 Phase의 산출물만을 전제로 한다.

| Phase | Task | Estimated Duration | Owner |
|---------|--------------------------|--------------------|--------|
| Phase 1 | **기반 골격**: Go 모듈, CLI 라우팅(표준 `flag` 기반), `config.json` 로드/저장, 레벨 로거. 검증: `hypervm-mcp.exe --help`가 전 서브커맨드를 출력한다. | 0.5일 | heavycaffeiner |
| Phase 2 | **PowerShell Runner**: 봉투 래핑, `-EncodedCommand` 인코딩, stdin 인자 주입, 타임아웃, 오류 코드 매핑, 동시 실행 세마포어. 검증: 관리자 셸에서 `Get-VM` 스크립트를 태워 실제 VM 목록 JSON을 얻는다(서비스 불필요). | 1일 | heavycaffeiner |
| Phase 3 | **파이프 전송 계층**: SDDL 조립, `winio.ListenPipe`, 연결당 goroutine, 제어/MCP 프레임 라우터, 브리지. 검증: 더미 에코 핸들러로 브리지→파이프→핸들러 왕복 확인. | 1일 | heavycaffeiner |
| Phase 4 | **Windows 서비스 호스트**: `service install/uninstall/start/stop/status/run`, SCM 연동, UAC 자기 재실행, ProgramData ACL, 바이너리 복사, vmms 의존성, 복구 정책, 파일+이벤트 로그. 검증: 설치 후 재부팅해도 자동 기동되고 비권한 브리지가 붙는다. | 1.5일 | heavycaffeiner |
| Phase 5 | **MCP 서버 및 VM 라이프사이클 도구**: MCP SDK 연결, 도구 등록 프레임워크, 게스트 IP 해석, 5-1-2의 8개 도구. 검증: Claude Code에서 실제 VM을 시작하고 `wait_for_guest_ip`로 IP를 받는다. | 1일 | heavycaffeiner |
| Phase 6 | **체크포인트 도구**: 5-1-3의 도구 4종. 검증: 체크포인트 생성 → 게스트 변경 → 롤백 → 삭제(머지 완료 대기 포함) 사이클. | 0.5일 | heavycaffeiner |
| Phase 7 | **경로 검증 및 저장 위치**: `ValidatePath`(매핑 드라이브·UNC·상위 디렉터리·여유 공간 판정), `get/set_host_storage_paths`, `create_vm`의 4종 경로 인자, `set_vm_storage_paths`. 검증: 매핑 드라이브 경로가 정확한 안내와 함께 거부되고, `D:\VMs`에 VM이 생성되며, 체크포인트가 지정 경로에 생긴다. | 1일 | heavycaffeiner |
| Phase 8 | **VHD 및 이미지 도구**: `create/attach/detach/resize/convert_vhd`, `get_vhd_info`, `create_vm_from_template`(차등 디스크), `export_vm`, `import_vm`, `delete_vm`의 디스크 공유 판정. 검증: 골든 VHDX로부터 VM 2대를 각각 10초 내에 프로비저닝하고, 내보내기 후 다른 경로로 가져오기가 성공한다. | 1.5일 | heavycaffeiner |
| Phase 9 | **네트워크 토폴로지**: `list_physical_adapters`, `list/create/delete_switch`(External 단절 경고 및 단일 NIC 가드), `set_vm_network`(정적 MAC·VLAN·MAC 스푸핑), `diagnose_vm_network`(호스트 점유 포트 판정 포함). 검증: External 스위치를 만들어 VM이 LAN DHCP로 `192.168.0.x`를 받고, LAN의 다른 기기에서 VM에 ping이 통한다. | 1.5일 | heavycaffeiner |
| Phase 10 | **자격증명 저장소 및 PowerShell Direct 게스트 도구**: DPAPI 머신 스코프, 원자적 교체, `cred` CLI, 제어 프레임 핸들러, 5-1-6의 도구 3종. 검증: 자격증명 등록 후 게스트 내부 명령 실행과 파일 전송. | 1일 | heavycaffeiner |
| Phase 11 | **SSH 계층 및 게스트 정적 IP**: `x/crypto/ssh` 클라이언트, TOFU known_hosts, 커넥션 풀과 keepalive, 5-1-7의 도구 3종, `set_guest_static_ip`(Windows=PowerShell Direct, Linux=nmcli/netplan + 자기 연결 절단 대응 폴링). 검증: Linux 게스트에 SSH로 명령 실행, VM 재생성 후 호스트키 불일치 검출과 `trust_new_key` 해소, 게스트에 정적 IP를 넣고 새 주소로 재접속 확인. | 2일 | heavycaffeiner |
| Phase 12 | **터널 매니저 (loopback)**: 터널 레지스트리, `direct`/`ssh` 데이터 경로, 백엔드 실패 시 IP 재해석, 커넥션 트래킹과 즉시 종료, `tunnels.json` 영속화 및 재시작 복원, 5-1-8의 `open/list/close`. 검증: 게스트 `0.0.0.0:8080`을 `localhost:8080`으로, 게스트 `127.0.0.1:5432`를 `ssh` 모드로 접근. 서비스 재시작 후 터널 생존. VM 재기동으로 IP가 바뀌어도 터널 복구. 445 요청이 External 스위치 안내와 함께 거부됨. | 2일 | heavycaffeiner |
| Phase 13 | **Tailnet 노출**: `tailscale status --json` 파싱, `tailnet`/`all` 바인드 스코프, 방화벽 규칙 생성/삭제 및 서비스 종료 시 정리, `tailscale_serve`, `tailnet_status`. 검증: 다른 tailnet 기기에서 `100.x.y.z:8080` 및 `https://<host>.<tailnet>.ts.net/`으로 VM 서비스 접근. 터널 종료 후 방화벽 규칙 잔존 없음. | 1.5일 | heavycaffeiner |
| Phase 14 | **진단 및 마감**: `doctor`(서비스·파이프·Hyper-V·SID·경로 접근성·스위치·Tailscale·터널/방화벽 정합성), `tunnel list` CLI, README(설치·설정·트러블슈팅·터널 대 External 스위치 판단 가이드·SMB 테스트 레시피), 오류 코드 표 정합성 점검, 비밀 로그 누출 감사. 검증: 초기화된 환경에서 README만 보고 설치부터 SMB 테스트와 tailnet 접근까지 완주. | 1일 | heavycaffeiner |

총 예상 기간: 17일

**권장 착수 순서.** Phase 1-5가 최소 동작 제품이며 여기까지만으로도 UAC 문제는 해소된다. 이후는 필요에 따라 Phase 7-8(저장 위치), Phase 9(SMB 등 LAN 테스트), Phase 12-13(터널·tailnet) 중 급한 것부터 진행해도 무방하다. 단 Phase 11(SSH)은 Phase 12의 `ssh` 모드가 의존하므로 선행되어야 한다.

### 6-2. Dependencies

#### 6-2-1. 라이브러리 의존성

| 모듈 | 버전 | 용도 |
|------|------|------|
| `github.com/modelcontextprotocol/go-sdk` | v1.7.0 | MCP 프로토콜 서버. `mcp.NewServer`, `mcp.AddTool`(Go 타입에서 JSON Schema 추론), 임의 `io.ReadWriteCloser` 위의 `IOTransport` |
| `github.com/Microsoft/go-winio` | v0.6.2 | Named Pipe 리스너/다이얼러. `PipeConfig.SecurityDescriptor`로 SDDL 직접 지정 |
| `golang.org/x/sys` | v0.47.0 | `windows/svc`(SCM 연동), `windows/svc/mgr`(설치/제거), `windows/svc/eventlog`, DPAPI(`CryptProtectData`), 토큰/SID API, `GetDriveType`, `ShellExecuteEx` |
| `golang.org/x/crypto` | v0.54.0 | `ssh` 클라이언트, 암호화된 개인키 파싱, 호스트키 지문 계산 |
| `golang.org/x/term` | v0.45.0 | `cred set`의 에코 없는 암호 입력 |

`gopkg.in/yaml.v3`는 사용하지 않는다. 설정 파일을 JSON으로 통일하여 표준 라이브러리만으로 처리한다.

#### 6-2-2. 런타임 의존성

- **Windows 10/11 Pro 또는 Enterprise, 혹은 Windows Server** — Hyper-V 역할 활성화 필요.
- **PowerShell 5.1** (`%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe`) — 사용하는 in-box 모듈: `Hyper-V`(VM·스위치·VHD), `NetSecurity`(`New-NetFirewallRule`), `NetAdapter`/`NetTCPIP`(물리 어댑터 조회, 게스트 정적 IP). PowerShell 7은 이들을 WinPSCompat 세션으로 로드하므로 예측 가능성을 위해 5.1을 기본으로 고정하고 `config.json`으로 재정의 가능하게 한다.
- **관리자 계정** — 서비스 설치 시 1회 필요.
- **게스트 통합 서비스** — `guest_copy_file`은 Guest Service Interface, `guest_invoke_command`는 PowerShell Direct 지원(Windows 게스트 Windows 10/Server 2016 이상), 게스트 IP 보고는 Data Exchange(KVP) 컴포넌트를 요구한다.
- **게스트 sshd** — `ssh_exec`, `ssh` 모드 터널, Linux 게스트의 `set_guest_static_ip`에 필요. 게스트에 이미 구성되어 있어야 한다. Windows 게스트라면 `guest_invoke_command`로 OpenSSH 서버를 부트스트랩할 수 있다.
- **Tailscale for Windows** — `tailnet` 바인드 스코프와 `tailscale_serve`에 필요. 호스트에 설치·로그인(`BackendState == "Running"`)되어 있어야 한다. 미설치 시 다른 기능은 정상 동작하며 해당 도구만 `TAILSCALE_UNAVAILABLE`을 반환한다.
- **가상 스위치** — 기능별 요구가 다르다.
  - PowerShell Direct 계열(5-1-6): 네트워크 불필요. **Private 스위치나 네트워크 없는 VM에서도 동작하는 유일한 경로.**
  - SSH 및 `direct` 모드 터널: 호스트가 게스트 IP로 라우팅 가능해야 하므로 Default Switch, External, 또는 Internal 필요. **Private 스위치는 불가.**
  - SMB 등 LAN 직접 접근(2.2.3): **External 스위치 필수.**
- **물리 네트워크 어댑터** — External 스위치 생성에 필요. 유선 권장(4.4.3의 무선 제약). LAN에 DHCP 서버가 있어야 게스트가 `192.168.0.x`를 자동으로 받는다.

#### 6-2-3. 검증 환경 의존성

- 개발 머신(`HYUNDESKTOP`)에 Go 1.26.5 설치 확인됨.
- Hyper-V PowerShell 모듈 존재 확인됨(`Get-VM` 명령 resolve 성공).
- Phase 6·10 검증: 폐기 가능한 테스트 VM 1대와 게스트 OS 로컬 계정.
- Phase 7·8 검증: 시스템 드라이브가 아닌 별도 볼륨(예: `D:`) 1개, 완성된 골든 VHDX 1개, 그리고 매핑 드라이브 1개(거부 경로 검증용).
- Phase 9 검증: 유선 물리 NIC 1개, LAN DHCP 서버, LAN에 속한 별도 기기 1대(게스트로 ping·SMB 접속용).
- Phase 11·12 검증: sshd가 동작하는 Linux 게스트 1대, 그리고 게스트 `0.0.0.0`에 바인딩된 서비스와 게스트 `127.0.0.1`에만 바인딩된 서비스 각각 하나(두 모드를 구분해 검증하기 위함).
- Phase 13 검증: 호스트가 로그인된 tailnet과 그 tailnet에 속한 별도 기기 1대.
- 통합 검증(Phase 14): 게스트에서 SMB 공유를 제공하는 VM 1대. External 스위치에 연결하여 호스트와 LAN 기기 양쪽에서 `\\192.168.0.x\share` 접근이 성공해야 한다.

#### 6-2-4. 타 팀 협업

없음. 단독 진행한다.

## 7. References

### MCP
- [Model Context Protocol Specification](https://modelcontextprotocol.io/specification) — MCP 프로토콜 규격
- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) — 서버 구현에 사용하는 공식 Go SDK
- [Claude Code MCP Configuration](https://docs.claude.com/en/docs/claude-code/mcp) — `.mcp.json` 설정 형식 및 stdio 서버 등록 방법

### Windows 권한 및 IPC
- [Security Descriptor Definition Language](https://learn.microsoft.com/en-us/windows/win32/secauthz/security-descriptor-definition-language) — 4.3.2 SDDL 문자열 문법
- [Named Pipe Security and Access Rights](https://learn.microsoft.com/en-us/windows/win32/ipc/named-pipe-security-and-access-rights) — 4.3.2 접근 마스크 `0x12019b`의 근거
- [go-winio PipeConfig](https://pkg.go.dev/github.com/Microsoft/go-winio#PipeConfig) — SDDL 지정 방식
- [golang.org/x/sys/windows/svc](https://pkg.go.dev/golang.org/x/sys/windows/svc) — Windows 서비스 호스트 구현
- [Service Security and Access Rights](https://learn.microsoft.com/en-us/windows/win32/services/service-security-and-access-rights) — 4.3.1 단계 4에서 바이너리를 ProgramData로 복사하는 이유
- [LocalSystem Account](https://learn.microsoft.com/en-us/windows/win32/services/localsystem-account) — 4.5.3 UNC 접근 시 머신 계정으로 인증되는 근거
- [CryptProtectData function](https://learn.microsoft.com/en-us/windows/win32/api/dpapi/nf-dpapi-cryptprotectdata) — 4.2.2 `CRYPTPROTECT_LOCAL_MACHINE` 플래그 의미

### Hyper-V
- [Hyper-V Cmdlets in Windows PowerShell](https://learn.microsoft.com/en-us/powershell/module/hyper-v/) — 4장·5장의 모든 스크립트가 참조하는 cmdlet 레퍼런스
- [Manage Windows with PowerShell Direct](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/manage/manage-windows-virtual-machines-with-powershell-direct) — 5-1-6 게스트 실행 도구의 근거
- [Hyper-V Integration Services](https://learn.microsoft.com/en-us/virtualization/hyper-v-on-windows/reference/integration-services) — 4.3.6 게스트 IP 보고(Data Exchange)와 Guest Service Interface의 근거
- [Set-VMHost](https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vmhost) — 4.5.1 호스트 기본 경로 변경
- [New-VM](https://learn.microsoft.com/en-us/powershell/module/hyper-v/new-vm) — 4.5.1 `-Path`가 VM 구성 파일 위치를 결정하며 이후 변경 불가한 근거
- [New-VHD](https://learn.microsoft.com/en-us/powershell/module/hyper-v/new-vhd) — 4.5.2 차등 디스크 생성
- [Export-VM / Import-VM](https://learn.microsoft.com/en-us/powershell/module/hyper-v/export-vm) — 5-1-4 내보내기·가져오기 및 구성 경로 이전
- [Hyper-V Virtual Switch types](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v-virtual-switch/hyper-v-virtual-switch) — 4.4.1 스위치 유형별 도달성 표의 근거
- [New-VMSwitch](https://learn.microsoft.com/en-us/powershell/module/hyper-v/new-vmswitch) — 4.4.3 External 스위치 생성과 `-AllowManagementOS`
- [Set-VMNetworkAdapter](https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vmnetworkadapter) — 5-1-5 정적 MAC 및 MAC 스푸핑

### 네트워크 및 게스트 접근
- [golang.org/x/crypto/ssh](https://pkg.go.dev/golang.org/x/crypto/ssh) — 4.6.1 SSH 클라이언트, `Client.Dial`을 통한 채널 포워딩
- [OpenSSH in Windows](https://learn.microsoft.com/en-us/windows-server/administration/openssh/openssh_install_firstuse) — Windows 게스트에 sshd를 부트스트랩하는 방법
- [New-NetIPAddress](https://learn.microsoft.com/en-us/powershell/module/nettcpip/new-netipaddress) — 4.4.4 Windows 게스트 정적 IP 설정
- [Netplan configuration](https://netplan.readthedocs.io/en/stable/netplan-yaml/) — 4.4.4 Linux 게스트 정적 IP 설정 (netplan 경로)
- [nmcli manual](https://networkmanager.dev/docs/api/latest/nmcli.html) — 4.4.4 Linux 게스트 정적 IP 설정 (NetworkManager 경로)
- [New-NetFirewallRule](https://learn.microsoft.com/en-us/powershell/module/netsecurity/new-netfirewallrule) — 4.6.2 방화벽 규칙 생성
- [Service overview and network port requirements for Windows](https://learn.microsoft.com/en-us/troubleshoot/windows-server/networking/service-overview-and-network-port-requirements) — 4.4.2 호스트가 점유하는 445·3389·5985 등의 근거

### Tailscale
- [Tailscale CLI: status](https://tailscale.com/kb/1080/cli#status) — 4.6.3 `--json` 출력 스키마와 `BackendState`
- [Tailscale Serve](https://tailscale.com/kb/1312/serve) — 5-1-8 `tailscale_serve` 도구의 근거
- [Tailscale IP address ranges](https://tailscale.com/kb/1015/100.x-addresses) — `100.64.0.0/10` CGNAT 대역과 `fd7a:115c:a1e0::/48` IPv6 ULA 대역
- [Tailscale subnet routers](https://tailscale.com/kb/1019/subnets) — 3.2에서 제외한 대안. `doctor`가 안내하는 근거
