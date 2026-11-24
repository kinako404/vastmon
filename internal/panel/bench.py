# vastmon 快捷算力检测：在实例上用现成的 PyTorch 跑，最后一行打印
# "VASTMON_BENCH {json}"。用来发现假卡、降频卡、虚标显存。
#
# 测的东西：
#   身份     torch / nvidia-smi 报的型号、SM 数、算力版本、显存、驱动
#   正确性   GPU 矩阵乘和 CPU 的结果比对（坏卡/假卡可能算错）
#   算力     FP16 / BF16 / FP32 矩阵乘 TFLOPS（FP32 关掉 TF32，测真 FP32）
#   带宽     显存到显存拷贝 GB/s（读 + 写都算）
#   显存     真的写满可用显存再逐块读回校验（虚标显存会在这里露馅）
#   功耗     读功耗上限（生效的 / 默认 / 最大），再满载跑约 6 秒 FP16，期间采样功耗、
#            SM 频率、温度：同型号的卡可能被宿主机限了功耗，或者一热就降频
#   健康     nvidia-smi 的坏页待隔离、ECC 不可纠正错误：有坏页待隔离的卡要重置后才能用，
#            CUDA 会报 "busy or unavailable"，nvidia-smi 却看着正常
#
# 环境变量 VASTMON_FORCE=1 时，GPU 正忙也照测（默认占用超过 20% 就不测，
# 免得打断正在跑的任务）。

import json
import os
import subprocess
import sys
import threading
import time

out = {"ok": False, "steps": {}}


def step(name, fn):
    t = time.time()
    try:
        out[name] = fn()
    except Exception as e:  # 单项失败不影响其它项
        out[name] = None
        out.setdefault("errors", {})[name] = f"{type(e).__name__}: {e}"[:400]
    out["steps"][name] = round(time.time() - t, 2)


def smi(query):
    r = subprocess.run(["nvidia-smi", "--query-gpu=" + query, "--format=csv,noheader,nounits"],
                       capture_output=True, text=True, timeout=30)
    rows = [[c.strip() for c in line.split(",")] for line in r.stdout.strip().splitlines() if line.strip()]
    keys = query.split(",")
    return [dict(zip(keys, row)) for row in rows]


def main():
    try:
        out["smi"] = smi("name,memory.total,memory.used,utilization.gpu,driver_version,"
                         "clocks.max.sm,power.limit,temperature.gpu,pcie.link.gen.current,pcie.link.width.current,"
                         "compute_mode,retired_pages.pending,ecc.errors.uncorrected.volatile.total,"
                         "ecc.errors.uncorrected.aggregate.total,enforced.power.limit,power.default_limit,"
                         "power.max_limit,power.draw,clocks.sm")
    except Exception as e:
        out["smi_error"] = str(e)[:200]

    import torch
    out["python"] = sys.executable
    out["torch"] = torch.__version__
    out["torch_cuda"] = torch.version.cuda
    if not torch.cuda.is_available():
        raise RuntimeError("PyTorch 看不到 CUDA")
    out["count"] = torch.cuda.device_count()
    p = torch.cuda.get_device_properties(0)
    out["name"] = p.name
    out["cc"] = f"{p.major}.{p.minor}"
    out["sms"] = p.multi_processor_count
    out["vram_gb"] = round(p.total_memory / 2**30, 2)

    busy = int((out.get("smi") or [{}])[0].get("utilization.gpu") or 0)
    if busy > 20 and os.environ.get("VASTMON_FORCE") != "1":
        raise RuntimeError(f"GPU 正忙（占用 {busy}%），先停掉任务再测，或强制检测")

    dev = torch.device("cuda:0")
    torch.backends.cuda.matmul.allow_tf32 = False
    torch.backends.cudnn.allow_tf32 = False
    torch.backends.cuda.matmul.allow_fp16_reduced_precision_reduction = False
    torch.backends.cuda.matmul.allow_bf16_reduced_precision_reduction = False
    # 第一次真正的 GPU 运算：架构不支持（no kernel image）会在这里报出来
    (torch.ones(4, device=dev) * 2).sum().item()

    def correctness():
        g = torch.Generator().manual_seed(0)
        a = torch.randn(1024, 1024, generator=g)
        b = torch.randn(1024, 1024, generator=g)
        ref = a.double() @ b.double()
        got = (a.to(dev) @ b.to(dev)).double().cpu()
        rel = ((got - ref).abs().max() / ref.abs().max()).item()
        h = (a.to(dev).half() @ b.to(dev).half()).double().cpu()
        rel16 = ((h - ref).abs().max() / ref.abs().max()).item()
        return {"fp32_rel_err": rel, "fp16_rel_err": rel16, "ok": rel < 1e-4 and rel16 < 2e-2}

    def matmul(dtype, n=8192, iters=10, rounds=3):
        a = torch.randn(n, n, device=dev, dtype=dtype)
        b = torch.randn(n, n, device=dev, dtype=dtype)
        c = torch.empty(n, n, device=dev, dtype=dtype)
        for _ in range(3):
            torch.matmul(a, b, out=c)
        torch.cuda.synchronize()
        best = float("inf")
        start, end = torch.cuda.Event(enable_timing=True), torch.cuda.Event(enable_timing=True)
        for _ in range(rounds):
            start.record()
            for _ in range(iters):
                torch.matmul(a, b, out=c)
            end.record()
            torch.cuda.synchronize()
            best = min(best, start.elapsed_time(end) / 1000 / iters)
        del a, b, c
        torch.cuda.empty_cache()
        return round(2 * n ** 3 / best / 1e12, 1)

    def copy_bw(size=1 << 30, iters=20, rounds=3):
        x = torch.empty(size, dtype=torch.uint8, device=dev).fill_(1)
        y = torch.empty_like(x)
        for _ in range(3):
            y.copy_(x)
        torch.cuda.synchronize()
        best = float("inf")
        start, end = torch.cuda.Event(enable_timing=True), torch.cuda.Event(enable_timing=True)
        for _ in range(rounds):
            start.record()
            for _ in range(iters):
                y.copy_(x)
            end.record()
            torch.cuda.synchronize()
            best = min(best, start.elapsed_time(end) / 1000 / iters)
        del x, y
        torch.cuda.empty_cache()
        return round(2 * size / best / 1e9, 1)

    def vram_fill():
        free, total = torch.cuda.mem_get_info()
        target = int(free * 0.92)
        chunk = 1 << 30
        blocks, done = [], 0
        try:
            while done + chunk <= target:
                t = torch.empty(chunk, dtype=torch.uint8, device=dev)
                t.fill_(len(blocks) % 251 + 1)
                blocks.append(t)
                done += chunk
        except RuntimeError:  # 实际分不出报告的那么多：多半是虚标
            pass
        torch.cuda.synchronize()
        bad = 0
        for i, t in enumerate(blocks):
            if not bool((t == (i % 251 + 1)).all()):
                bad += 1
        res = {"free_gb": round(free / 2**30, 2), "target_gb": round(target / 2**30, 2),
               "filled_gb": round(done / 2**30, 2), "bad_blocks": bad,
               "ok": bad == 0 and done >= target - chunk}
        del blocks
        torch.cuda.empty_cache()
        return res

    step("correct", correctness)
    step("fp16", lambda: matmul(torch.float16))
    if torch.cuda.is_bf16_supported():
        step("bf16", lambda: matmul(torch.bfloat16))
    else:
        out["bf16"] = None
        out.setdefault("skipped", []).append("bf16（这张卡不支持）")
    step("fp32", lambda: matmul(torch.float32, n=4096 if out.get("fp16") and out["fp16"] < 40 else 8192))
    step("copy_gbs", copy_bw)
    step("sustain", lambda: sustain(dev))
    step("vram", vram_fill)
    out["ok"] = True


class Sampler:
    """后台每 0.25 秒读一次 GPU 0 的功耗、SM 频率、温度。"""

    def __init__(self):
        self.rows = []
        self.stop = threading.Event()
        self.t = threading.Thread(target=self.run, daemon=True)

    def run(self):
        while not self.stop.is_set():
            try:
                line = subprocess.run(["nvidia-smi", "-i", "0", "--query-gpu=power.draw,clocks.sm,temperature.gpu",
                                       "--format=csv,noheader,nounits"], capture_output=True, text=True,
                                      timeout=5).stdout.strip().splitlines()[0]
                self.rows.append([float(x) for x in line.split(",")])
            except Exception:
                pass
            self.stop.wait(0.25)

    def __enter__(self):
        self.t.start()
        return self

    def __exit__(self, *exc):
        self.stop.set()
        self.t.join(timeout=6)


def sustain(dev, seconds=6.0, n=8192):
    """满载跑 FP16 矩阵乘，量持续算力和满载功耗、频率、温度。"""
    import torch
    a = torch.randn(n, n, device=dev, dtype=torch.float16)
    b = torch.randn(n, n, device=dev, dtype=torch.float16)
    c = torch.empty(n, n, device=dev, dtype=torch.float16)
    torch.matmul(a, b, out=c)
    torch.cuda.synchronize()
    with Sampler() as s:
        t0 = time.time()
        count = 0
        while time.time() - t0 < seconds:
            for _ in range(5):
                torch.matmul(a, b, out=c)
            torch.cuda.synchronize()
            count += 5
        dt = time.time() - t0
    del a, b, c
    torch.cuda.empty_cache()
    rows = s.rows[len(s.rows) // 3:] or s.rows  # 去掉刚升功耗的前三分之一
    res = {"tflops": round(2 * n ** 3 * count / dt / 1e12, 1), "seconds": round(dt, 1), "samples": len(s.rows)}
    if rows:
        res["power_peak_w"] = round(max(r[0] for r in s.rows), 1)
        res["power_avg_w"] = round(sum(r[0] for r in rows) / len(rows), 1)
        res["clock_sm_mhz"] = round(sum(r[1] for r in rows) / len(rows))
        res["temp_max_c"] = round(max(r[2] for r in s.rows))
    return res


t0 = time.time()
try:
    main()
except Exception as e:
    out["error"] = f"{type(e).__name__}: {e}"[:600]
out["seconds"] = round(time.time() - t0, 1)
print("VASTMON_BENCH " + json.dumps(out, ensure_ascii=False))
sys.stdout.flush()
