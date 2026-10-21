"""一键部署 webvpn 到 creds.json 里配置的机器，并重启服务。

做了这些事：

  1. 本地交叉编译 linux/amd64 二进制
  2. 备份服务器上的二进制、systemd 单元和配置（带时间戳，回滚用）
  3. 上传新二进制，先拿**服务器上现有的配置**跑一次 -check。新二进制不认旧
     配置的话在这一步就停，机器上什么都没动过
  4. 服务器配置里没有 lan_index / state 段时补齐（幂等）。**只追加，不覆盖**，
     已有的 services 和其他内容一个字节都不碰。state 段是必须的：新二进制
     默认把状态放在配置同目录，也就是只读的 /opt/webvpn，不指到
     /var/lib/webvpn 的话服务会起不来
  5. 上传 systemd 单元、daemon-reload、原子替换二进制、重启
  6. 验证：服务 active、80 绑在内网 IP 上而不是 0.0.0.0、curl 拿得到索引页、
     /var/lib/webvpn/state.json 已生成且属主权限正确

任何一步失败都会把二进制/单元/配置回滚回去，再打印日志和回滚点。

刻意不做的事：

  * **不上传本地的 config.yaml。** 服务器那份才是准的（本地少两个服务），
    而且里面有真实的签名密钥和密码哈希。配置改动一律是「远程追加」。
  * **不碰 state.json。** 那里面是运行时改出来的服务清单、用户和权限，
    是真正的现状。第一次部署时它还不存在，由服务自己播种生成。
  * **不写死密码。** 从 creds.json 读，顺序见 CREDS_PATHS；也可用环境变量
    WEBVPN_CREDS 指定。密码不出现在这个文件里。

用法：

    python push.py
"""

import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

import paramiko

HERE = Path(__file__).resolve().parent
REMOTE_DIR = "/opt/webvpn"
REMOTE_BIN = f"{REMOTE_DIR}/webvpn"
REMOTE_CONFIG = f"{REMOTE_DIR}/config.yaml"
# 由 webvpn.service 里的 StateDirectory=webvpn 创建并授权。不能放进
# /opt/webvpn：ProtectSystem=strict 让那里是只读的，服务会在启动写状态时
# 失败退出，而那个错误在 journalctl 里看着像权限问题、不像路径问题。
REMOTE_STATE = "/var/lib/webvpn/state.json"
UNIT_PATH = "/etc/systemd/system/webvpn.service"
SERVICE = "webvpn.service"

# 凭据查找顺序：
#   1. 环境变量 WEBVPN_CREDS 指向的文件
#   2. 项目目录下的 creds.json（已被 .gitignore 忽略）
#   3. ~/.config/webvpn/creds.json
#
# creds.json 是 {"host": ..., "port": 22, "user": ..., "password": ...}。
# 密码不要写进这个文件，也不要提交到仓库。
CREDS_PATHS = [
    Path(os.environ["WEBVPN_CREDS"]) if os.environ.get("WEBVPN_CREDS") else None,
    HERE / "creds.json",
    Path.home() / ".config" / "webvpn" / "creds.json",
]

for _s in (sys.stdout, sys.stderr):
    try:
        _s.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass


def load_creds():
    for path in CREDS_PATHS:
        if path and path.is_file():
            print(f"凭据：{path}")
            return json.loads(path.read_text(encoding="utf-8"))
    tried = ", ".join(str(p) for p in CREDS_PATHS if p)
    sys.exit(f"找不到 creds.json。找过：{tried}\n用 WEBVPN_CREDS=<路径> 指定。")


def run(client, command, timeout=120):
    """跑一条远程命令，输出原样打出来，返回退出码。"""
    _, stdout, stderr = client.exec_command(command, timeout=timeout)
    out = stdout.read().decode("utf-8", errors="replace").strip()
    err = stderr.read().decode("utf-8", errors="replace").strip()
    code = stdout.channel.recv_exit_status()
    print(f"$ {command}")
    for line in (out, err):
        if line:
            print("  " + line.replace("\n", "\n  "))
    return code


def capture(client, command, timeout=120):
    """跑一条远程命令，只返回 stdout 文本（用于要判断内容的场合）。"""
    _, stdout, _ = client.exec_command(command, timeout=timeout)
    out = stdout.read().decode("utf-8", errors="replace")
    stdout.channel.recv_exit_status()
    return out


def build():
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    out = HERE / "webvpn-linux-amd64"
    r = subprocess.run(
        ["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", str(out), "."],
        cwd=HERE, env=env, capture_output=True, text=True,
    )
    if r.returncode != 0:
        print(r.stdout + r.stderr)
        sys.exit("编译失败，未连服务器。")
    print(f"  -> {out.name}  {out.stat().st_size} bytes")
    return out


def lan_index_block(host):
    return f"""
# 内网索引页：http://{host}/ ，免登录，卡片直接链到各服务的内网地址。
# 管理页也挂在这个监听器上，路径 /admin。绑内网 IP 而不是 0.0.0.0 —— 这台
# 机器有公网地址，绑宽了等于把服务清单和管理入口一起公开。
# 80 是特权端口，靠 webvpn.service 里的 CAP_NET_BIND_SERVICE。
lan_index:
  listen: "{host}:80"
  host: "{host}"
"""


def state_block(path):
    return f"""
# 运行时可变的那部分：服务清单、用户、以及每个用户能访问哪些站点。管理页
# 改的是这个文件，不是 config.yaml。
#
# 路径必须显式写死。默认值是「配置文件同目录」，也就是只读的 /opt/webvpn，
# 服务会在启动时写状态失败而退出。目录由单元里的 StateDirectory 建好（0700）。
#
# 下面 config.yaml 里的 services: 和 users: 从此只是首次播种用的种子；文件
# 存在之后一切以它为准，删掉的用户不会因为重启而复活。
state:
  path: "{path}"
"""


def has_top_level(text, key):
    """配置里有没有某个顶层键。按行首匹配，免得被注释或别的键名里的
    同名词（比如某处注释提到 state:）骗过去。"""
    return re.search(rf"^{re.escape(key)}:", text, re.M) is not None


def append_block(text, block):
    if not text.endswith("\n"):
        text += "\n"
    return text + block


def main():
    creds = load_creds()
    host = creds["host"]

    print("\n[1/7] 编译 linux/amd64 二进制")
    binary = build()

    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(
        host, port=creds.get("port", 22), username=creds["user"],
        password=creds["password"], timeout=20,
        look_for_keys=False, allow_agent=False,
    )
    sftp = client.open_sftp()

    stamp = time.strftime("%Y%m%d-%H%M%S")
    bin_backup = f"{REMOTE_BIN}.bak-{stamp}"
    unit_backup = f"{UNIT_PATH}.bak-{stamp}"
    cfg_backup = f"{REMOTE_CONFIG}.bak-{stamp}"
    touched_config = False
    replaced_bin = False
    replaced_unit = False

    def rollback(reason):
        print(f"\n!! {reason}，正在回滚")
        if replaced_bin:
            run(client, f"cp -p {bin_backup} {REMOTE_BIN}")
        if replaced_unit:
            run(client, f"cp -p {unit_backup} {UNIT_PATH}")
        if touched_config:
            run(client, f"cp -p {cfg_backup} {REMOTE_CONFIG} && "
                        f"chown webvpn:webvpn {REMOTE_CONFIG} && chmod 600 {REMOTE_CONFIG}")
        run(client, f"systemctl daemon-reload; systemctl restart {SERVICE}; "
                    f"sleep 2; systemctl is-active {SERVICE}")
        print(f"  回滚点：{bin_backup} / {unit_backup} / {cfg_backup}")
        sftp.close()
        client.close()
        sys.exit(1)

    try:
        # --- 备份 -------------------------------------------------------
        print("\n[2/7] 备份二进制、单元、配置")
        if run(client, f"cp -p {REMOTE_BIN} {bin_backup} && "
                       f"cp -p {UNIT_PATH} {unit_backup} && "
                       f"cp -p {REMOTE_CONFIG} {cfg_backup} && echo backed-up") != 0:
            rollback("备份失败")
        print(f"  -> {stamp}\n")

        # --- 上传新二进制，先用旧配置体检 --------------------------------
        print("[3/7] 上传新二进制并用现有配置 -check")
        sftp.put(str(binary), f"{REMOTE_BIN}.new")
        sftp.chmod(f"{REMOTE_BIN}.new", 0o755)
        if run(client, f"{REMOTE_BIN}.new -check -config {REMOTE_CONFIG}") != 0:
            rollback("新二进制不接受服务器上现有的配置")

        # --- 补 lan_index / state（幂等，只追加）--------------------------
        print("\n[4/7] 检查 lan_index 与 state 配置段")
        remote_cfg = sftp.open(REMOTE_CONFIG, "r")
        text = remote_cfg.read().decode("utf-8")
        remote_cfg.close()

        added = []
        if not has_top_level(text, "lan_index"):
            text = append_block(text, lan_index_block(host))
            added.append("lan_index")
        if not has_top_level(text, "state"):
            text = append_block(text, state_block(REMOTE_STATE))
            added.append("state")

        if not added:
            print("  两段都已存在，跳过（不覆盖）")
        else:
            touched_config = True
            # 先写临时文件再 mv，避免中途失败把配置截断成半截。
            staging = f"{REMOTE_CONFIG}.new"
            with sftp.open(staging, "w") as f:
                f.write(text)
            # SFTP 以 root 写入会把属主变成 root:root，服务随即读不到配置
            # （600 + root 属主），表现为「改了配置起不来」的权限错误。
            if run(client, f"chown webvpn:webvpn {staging} && chmod 600 {staging} && "
                           f"mv {staging} {REMOTE_CONFIG} && echo written") != 0:
                rollback("写入配置失败")
            print(f"  已追加 {'、'.join(added)} 段")
            # 加完再体检一次：有服务 target 是回环却没配 host 的话，这里会报错。
            # 加了 state 之后这一跑还会顺带读出 /var/lib/webvpn/state.json
            # （若已存在）并校验，等于把「新二进制读不读得动老状态」提前到重启前。
            if run(client, f"{REMOTE_BIN}.new -check -config {REMOTE_CONFIG}") != 0:
                rollback("追加配置段之后校验不过")

        # --- 单元 + 换二进制 + 重启 --------------------------------------
        print("\n[5/7] 更新 systemd 单元并重启")
        sftp.put(str(HERE / "deploy" / "webvpn.service"), UNIT_PATH)
        sftp.chmod(UNIT_PATH, 0o644)
        replaced_unit = True
        run(client, "systemctl daemon-reload")
        if run(client, f"mv {REMOTE_BIN}.new {REMOTE_BIN} && "
                       f"chown webvpn:webvpn {REMOTE_BIN} && chmod 755 {REMOTE_BIN} && echo swapped") != 0:
            rollback("替换二进制失败")
        replaced_bin = True

        if run(client, f"systemctl restart {SERVICE} && sleep 2 && systemctl is-active {SERVICE}") != 0:
            print("\n服务没起来，最近的日志：")
            run(client, f"journalctl -u {SERVICE} -n 30 --no-pager")
            rollback("服务重启失败")

        # --- 验证 -------------------------------------------------------
        print("\n[6/7] 验证监听地址")
        ss = capture(client, "ss -lntp | grep -E ':(80|18790) '").strip()
        print("$ ss -lntp | grep -E ':(80|18790) '")
        print("  " + ss.replace("\n", "\n  ") if ss else "  (没有输出)")
        if ":80 " not in ss:
            rollback("80 端口没在监听")
        # 绑成通配是这个功能唯一真正危险的失误：内网索引页会变成公网可读。
        if "0.0.0.0:80" in ss or "*:80" in ss:
            rollback("80 绑在了通配地址上，索引页会暴露到公网")

        print("\n[7/7] 验证索引页与管理页")
        code = capture(client, f"curl -s -o /dev/null -w '%{{http_code}}' http://{host}/").strip()
        body = capture(client, f"curl -s http://{host}/")
        if code != "200":
            print(f"  警告：curl 返回 {code!r}，不是 200。服务是起来的，页面可能有问题；")
            print(f"        人工看一眼：curl -s http://{host}/ | head")
        elif 'class="card"' not in body:
            print("  警告：页面里没有卡片，可能一个服务都没渲染出来；")
            print(f"        人工看一眼：curl -s http://{host}/ | head -60")
        else:
            print(f"  HTTP 200，页面里有 {body.count('class="card"')} 张卡片")

        # 管理页未登录应当 302 跳登录，不是 200 也不是 404。200 意味着门没锁，
        # 404 意味着路由没挂上——两种情况都值得当场知道，而不是等哪天才发现。
        admin_code = capture(
            client, f"curl -s -o /dev/null -w '%{{http_code}}' http://{host}/admin").strip()
        if admin_code == "302":
            print("  /admin 未登录跳转到登录页（302）")
        else:
            print(f"  警告：/admin 返回 {admin_code!r}，期望 302。人工看一眼。")

        # 状态文件得真的写出来了，属主和权限也得对——那是密码哈希所在的文件。
        stat = capture(client, f"stat -c '%U:%G %a' {REMOTE_STATE} 2>/dev/null").strip()
        if not stat:
            print(f"  警告：{REMOTE_STATE} 不存在。服务是 active 的话它应该已经播种了；")
            print(f"        查一下：systemctl status {SERVICE}; ls -l /var/lib/webvpn/")
        elif stat != "webvpn:webvpn 600":
            print(f"  警告：{REMOTE_STATE} 的属主/权限是 {stat!r}，期望 'webvpn:webvpn 600'")
        else:
            print(f"  {REMOTE_STATE} 已生成，{stat}")

        print(f"\n部署完成。")
        print(f"  索引页（内网，免登录）：http://{host}/")
        print(f"  管理页（内网，需登录）：http://{host}/admin")
        print(f"  回滚点：{bin_backup} / {unit_backup} / {cfg_backup}")
    finally:
        try:
            sftp.close()
        except Exception:
            pass
        client.close()


if __name__ == "__main__":
    main()
