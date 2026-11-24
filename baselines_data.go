package main

// builtinBaselinesVersion 改了内置数据就加一，启动时会换掉库里的内置行。
const builtinBaselinesVersion = 2

// 规格来源（2026-11 查证）：
const (
	srcPro6000WS     = "https://www.nvidia.com/en-us/products/workstations/professional-desktop-gpus/rtx-pro-6000/"
	srcPro6000MaxQ   = "https://www.pny.com/nvidia-rtx-pro-6000-blackwell-max-q https://www.techpowerup.com/gpu-specs/rtx-pro-6000-blackwell-max-q-workstation.c4273"
	srcPro6000Server = "https://www.nvidia.com/en-us/data-center/rtx-pro-6000-blackwell-server-edition/ https://lenovopress.lenovo.com/lp2263.pdf"
	srcPro5000       = "https://www.nvidia.com/en-us/products/workstations/professional-desktop-gpus/rtx-pro-5000/ https://www.techpowerup.com/gpu-specs/rtx-pro-5000-blackwell-workstation.c4276"
	srcGeForce       = "https://www.techpowerup.com/gpu-specs/gigabyte-rtx-5090-gaming.b13160 https://pcper.com/2022/10/nvidia-geforce-rtx-4090-founders-edition-review/"
	srcRTX6000Ada    = "https://www.runpod.io/articles/guides/nvidia-rtx-6000-ada"
	srcA6000         = "https://www.thundercompute.com/blog/nvidia-rtx-a6000-specs"
	srcL40S          = "https://www.nvidia.com/en-us/data-center/l40s/ https://flopper.io/compare/nvidia-a40-48gb-vs-nvidia-l40s-48gb"
	srcA40           = "https://cloud-gpus.com/nvidia-a40 https://flopper.io/compare/nvidia-a40-48gb-vs-nvidia-l40s-48gb"
	srcA100          = "https://www.nvidia.com/en-us/data-center/a100/ https://aiinfra.blog/blog/aiinfra-365/day-28-reading-spec-sheets/"
	srcH100          = "https://www.nvidia.com/en-us/data-center/h100/ https://lenovopress.lenovo.com/lp1732-thinksystem-nvidia-h100-pcie-gen5-gpu"
	srcH100NVL       = "https://www.pny.com/file%20library/company/support/product%20brochures/nvidia%20data%20center%20gpus/english/h100-nvl-datasheet.pdf"
	srcH200          = "https://www.nvidia.com/en-us/data-center/h200/ https://www.spheron.network/blog/nvidia-h200-specs/"
	srcV100          = "https://wayhost.cloud/en/blog/nvidia-v100-gpu https://cputronic.com/en/gpu/nvidia-tesla-v100-sxm2-32-gb"
	srcCC            = "https://developer.nvidia.com/cuda-gpus"
)

const smNote = "SM 数由 CUDA 核心数推算（Ampere/Ada/Blackwell/Hopper 每 SM 128 核，Volta 64 核）"

// builtinBaselines 是内置的性能基准：规格来自上面的来源，算力版本来自 NVIDIA CUDA GPUs 表。
// 实测参考只放真测过的：RTX PRO 6000 WS 600W 那组是用户在一张正常卡上测的（2026-11）。
var builtinBaselines = []Baseline{
	{GPUName: "RTX PRO 6000 WS", FullName: "NVIDIA RTX PRO 6000 Blackwell Workstation Edition", PowerW: 600, SMs: 188, CC: "12.0",
		VRAMGB: 96, MemBWGBs: 1792, FP32Spec: 125, FP16: 170.6, BF16: 179.1, FP32: 30.3, CopyGBs: 1466.4,
		MeasuredSource: "用户在正常卡上实测（2026-11）", SpecSource: srcPro6000WS + " " + srcCC, Note: "24064 CUDA 核；" + smNote},
	{GPUName: "RTX PRO 6000 Max-Q", FullName: "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition", PowerW: 300, SMs: 188, CC: "12.0",
		VRAMGB: 96, MemBWGBs: 1792, SpecSource: srcPro6000MaxQ + " " + srcCC, Note: "和 WS 同一颗 GB202，功耗 300W；" + smNote},
	{GPUName: "RTX PRO 6000 S", FullName: "NVIDIA RTX PRO 6000 Blackwell Server Edition", PowerW: 600, SMs: 188, CC: "12.0",
		VRAMGB: 96, MemBWGBs: 1597, SpecSource: srcPro6000Server + " " + srcCC, Note: "被动散热，功耗可配置、最高 600W；带宽比 WS 低；" + smNote},
	{GPUName: "RTX PRO 5000", Variant: "48GB", FullName: "NVIDIA RTX PRO 5000 Blackwell", PowerW: 300, SMs: 110, CC: "12.0",
		VRAMGB: 48, MemBWGBs: 1344, SpecSource: srcPro5000 + " " + srcCC, Note: "另有 72GB 版"},
	{GPUName: "RTX 5090", FullName: "NVIDIA GeForce RTX 5090", PowerW: 575, SMs: 170, CC: "12.0",
		VRAMGB: 32, MemBWGBs: 1792, SpecSource: srcGeForce + " " + srcCC},
	{GPUName: "RTX 4090", FullName: "NVIDIA GeForce RTX 4090", PowerW: 450, SMs: 128, CC: "8.9",
		VRAMGB: 24, MemBWGBs: 1008, SpecSource: srcGeForce + " " + srcCC, Note: "带宽 1008 GB/s（部分资料写 1018）；市面上有改 48GB 的魔改卡"},
	{GPUName: "RTX 3090", FullName: "NVIDIA GeForce RTX 3090", PowerW: 350, SMs: 82, CC: "8.6",
		VRAMGB: 24, MemBWGBs: 936, SpecSource: srcGeForce + " " + srcCC},
	{GPUName: "RTX 6000Ada", FullName: "NVIDIA RTX 6000 Ada Generation", PowerW: 300, SMs: 142, CC: "8.9",
		VRAMGB: 48, MemBWGBs: 960, SpecSource: srcRTX6000Ada + " " + srcCC, Note: "18176 CUDA 核；" + smNote},
	{GPUName: "RTX A6000", FullName: "NVIDIA RTX A6000", PowerW: 300, SMs: 84, CC: "8.6",
		VRAMGB: 48, MemBWGBs: 768, SpecSource: srcA6000 + " " + srcCC, Note: "10752 CUDA 核；" + smNote},
	{GPUName: "L40S", FullName: "NVIDIA L40S", PowerW: 350, SMs: 142, CC: "8.9",
		VRAMGB: 48, MemBWGBs: 864, FP32Spec: 91.6, SpecSource: srcL40S + " " + srcCC},
	{GPUName: "A40", FullName: "NVIDIA A40", PowerW: 300, SMs: 84, CC: "8.6",
		VRAMGB: 48, MemBWGBs: 696, SpecSource: srcA40 + " " + srcCC, Note: "10752 CUDA 核；" + smNote},
	{GPUName: "A100 PCIE", Variant: "80GB", FullName: "NVIDIA A100 80GB PCIe", PowerW: 300, SMs: 108, CC: "8.0",
		VRAMGB: 80, MemBWGBs: 1935, FP32Spec: 19.5, SpecSource: srcA100 + " " + srcCC},
	{GPUName: "A100 PCIE", Variant: "40GB", FullName: "NVIDIA A100 40GB PCIe", PowerW: 250, SMs: 108, CC: "8.0",
		VRAMGB: 40, MemBWGBs: 1555, FP32Spec: 19.5, SpecSource: srcA100 + " " + srcCC},
	{GPUName: "A100 SXM4", Variant: "80GB", FullName: "NVIDIA A100 80GB SXM4", PowerW: 400, SMs: 108, CC: "8.0",
		VRAMGB: 80, MemBWGBs: 2039, FP32Spec: 19.5, SpecSource: srcA100 + " " + srcCC},
	{GPUName: "A100 SXM4", Variant: "40GB", FullName: "NVIDIA A100 40GB SXM4", PowerW: 400, SMs: 108, CC: "8.0",
		VRAMGB: 40, MemBWGBs: 1555, FP32Spec: 19.5, SpecSource: srcA100 + " " + srcCC},
	{GPUName: "H100 SXM", FullName: "NVIDIA H100 SXM5 80GB", PowerW: 700, SMs: 132, CC: "9.0",
		VRAMGB: 80, MemBWGBs: 3350, SpecSource: srcH100 + " " + srcCC},
	{GPUName: "H100 PCIE", FullName: "NVIDIA H100 PCIe 80GB", PowerW: 350, SMs: 114, CC: "9.0",
		VRAMGB: 80, MemBWGBs: 2000, SpecSource: srcH100 + " " + srcCC},
	{GPUName: "H100 NVL", FullName: "NVIDIA H100 NVL 94GB", PowerW: 400, SMs: 132, CC: "9.0",
		VRAMGB: 94, MemBWGBs: 3900, SpecSource: srcH100NVL + " " + srcCC, Note: "功耗 350–400W 可配置"},
	{GPUName: "H200", FullName: "NVIDIA H200 SXM 141GB", PowerW: 700, SMs: 132, CC: "9.0",
		VRAMGB: 141, MemBWGBs: 4800, FP32Spec: 67, SpecSource: srcH200 + " " + srcCC, Note: "16896 CUDA 核；" + smNote},
	{GPUName: "Tesla V100", Variant: "SXM2 32GB", FullName: "NVIDIA Tesla V100 SXM2 32GB", PowerW: 300, SMs: 80, CC: "7.0",
		VRAMGB: 32, MemBWGBs: 900, FP32Spec: 15.7, SpecSource: srcV100, Note: "5120 CUDA 核；" + smNote + "；算力版本 7.0 为实测"},
	{GPUName: "Tesla V100", Variant: "PCIe 32GB", FullName: "NVIDIA Tesla V100 PCIe 32GB", PowerW: 250, SMs: 80, CC: "7.0",
		VRAMGB: 32, MemBWGBs: 900, FP32Spec: 14, SpecSource: srcV100, Note: "也有 16GB 版"},
}
