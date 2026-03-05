#!/bin/bash

# RK3588 System Information Collector
# Output Format: Markdown
# Target: Armbian on Rockchip RK3588

OUTPUT_FILE="rk3588_system_report_$(date +%Y%m%d_%H%M%S).md"

# Color definitions for terminal echo (optional, not in markdown)
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

echo -e "${GREEN}Starting RK3588 System Information Collection...${NC}"

# Function to safely read file content
read_file() {
    local file=$1
    local default=$2
    if [ -f "$file" ]; then
        cat "$file" 2>/dev/null | tr -d '\n'
    else
        echo "$default"
    fi
}

# Start Markdown Output
{
echo "# í ½í³‹ RK3588 Development Board System Report"
echo ""
echo "**Generated Time:** $(date '+%Y-%m-%d %H:%M:%S %Z')"
echo "**Hostname:** $(hostname)"
echo ""

# 1. System & Kernel Info
echo "## 1. í ½í¶¥ï¸ System & Kernel"
echo ""
echo "| Item | Value |"
echo "| :--- | :--- |"
echo "| **OS** | $(cat /etc/os-release | grep PRETTY_NAME | cut -d'"' -f2) |"
echo "| **Kernel** | $(uname -r) |"
echo "| **Architecture** | $(uname -m) |"
echo "| **Uptime** | $(uptime -p 2>/dev/null || uptime | awk -F, '{print $1}' | awk '{print $3, $4}') |"
echo "| **Load Average** | $(cat /proc/loadavg | awk '{print $1, $2, $3}') |"
echo ""

# 2. SoC & Hardware Info
echo "## 2. í ½í²Ž SoC & Hardware (RK3588)"
echo ""
echo "| Item | Value |"
echo "| :--- | :--- |"

# Detect CPU Name
CPU_NAME=$(cat /proc/device-tree/model 2>/dev/null || echo "Unknown Model")
echo "| **Board Model** | $CPU_NAME |"

# Frequency (Current Max)
if [ -f /sys/devices/system/cpu/cpufreq/policy0/scaling_cur_freq ]; then
    CUR_FREQ=$(($(cat /sys/devices/system/cpu/cpufreq/policy0/scaling_cur_freq) / 1000))
    MAX_FREQ=$(($(cat /sys/devices/system/cpu/cpufreq/policy0/scaling_max_freq) / 1000))
    echo "| **CPU Freq** | Current: ${CUR_FREQ} MHz / Max: ${MAX_FREQ} MHz |"
else
    echo "| **CPU Freq** | N/A |"
fi

# GPU/Mali Info (if available)
if [ -d /sys/class/devfreq/gpu ]; then
    GPU_FREQ=$(cat /sys/class/devfreq/gpu/cur_freq 2>/dev/null)
    if [ -n "$GPU_FREQ" ]; then
        echo "| **GPU Freq** | $(($GPU_FREQ / 1000000)) MHz |"
    fi
fi

# NPU Info (Rockchip NPU driver usually exposes this)
if [ -f /sys/class/rknpu/rknpu0/clk_rate ]; then
    NPU_FREQ=$(($(cat /sys/class/rknpu/rknpu0/clk_rate) / 1000000))
    echo "| **NPU Freq** | ${NPU_FREQ} MHz |"
elif [ -f /sys/kernel/debug/rknpu/frequency ]; then
     NPU_FREQ=$(cat /sys/kernel/debug/rknpu/frequency 2>/dev/null)
     echo "| **NPU Freq** | ${NPU_FREQ:-Unknown} MHz |"
fi

echo ""

# 3. Memory & Swap
echo "## 3. í ½í²¾ Memory & Storage"
echo ""
echo "### Memory Usage"
echo '```text'
free -h
echo '```'
echo ""

# ZRAM specific check (common in Armbian)
if command -v zramctl &> /dev/null; then
    echo "### ZRAM Status"
    echo '```text'
    zramctl
    echo '```'
    echo ""
fi

# Storage
echo "### Block Devices"
echo '```text'
lsblk -o NAME,SIZE,TYPE,MOUNTPOINTS,FSTYPE
echo '```'
echo ""

# eMMC Health (if mmcblk0 is eMMC)
if [ -f /sys/class/mmc_host/mmc0/mmc0:0001/life_time_est_typ_a ]; then
    EMMC_LIFE_A=$(cat /sys/class/mmc_host/mmc0/mmc0:0001/life_time_est_typ_a 2>/dev/null)
    EMMC_LIFE_B=$(cat /sys/class/mmc_host/mmc0/mmc0:0001/life_time_est_typ_b 2>/dev/null)
    echo "> **eMMC Health Estimate:** Type A: $EMMC_LIFE_A, Type B: $EMMC_LIFE_B (0x01-0x0A represents 10%-100%)"
    echo ""
fi

# 4. Network Interfaces
echo "## 4. í ¼í¼ Network Interfaces"
echo ""
echo "| Interface | MAC Address | Status | IP Address | Driver/Bus |"
echo "| :--- | :--- | :--- | :--- | :--- |"

for iface in $(ls /sys/class/net/ | grep -v lo); do
    MAC=$(cat /sys/class/net/$iface/address 2>/dev/null)
    STATE=$(cat /sys/class/net/$iface/operstate 2>/dev/null)
    IP=$(ip -4 addr show $iface 2>/dev/null | grep inet | awk '{print $2}' | head -n1)
    IP=${IP:-"-"}
    
    # Try to find driver/bus info
    BUS_INFO=""
    if [ -d "/sys/class/net/$iface/device" ]; then
        DRIVER=$(readlink /sys/class/net/$iface/device/driver 2>/dev/null | xargs basename 2>/dev/null)
        BUS_INFO="${DRIVER:-unknown}"
        
        # Check PCI
        if [ -L "/sys/class/net/$iface/device" ]; then
            PCI_SLOT=$(readlink /sys/class/net/$iface/device 2>/dev/null | grep -oE '[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$')
            if [ -n "$PCI_SLOT" ]; then
                PCI_DEV=$(lspci -nn -s $PCI_SLOT 2>/dev/null | sed 's/.*\[//' | sed 's/\].*//')
                BUS_INFO="$BUS_INFO ($PCI_DEV)"
            fi
        fi
    fi
    
    echo "| $iface | $MAC | $STATE | $IP | $BUS_INFO |"
done
echo ""

# WiFi Specific (Broadcom/RTL based on your log)
if command -v iw &> /dev/null; then
    echo "### Wireless Scan Capability"
    echo '```text'
    iw dev 2>/dev/null | head -20
    echo '```'
    echo ""
fi

# 5. USB Devices
echo "## 5. í ½í´Œ USB Devices"
echo ""
echo '```text'
lsusb -t
echo '```'
echo ""
echo "### Detailed USB List"
echo '```text'
lsusb
echo '```'
echo ""

# 6. Thermal & Power
echo "## 6. í ¼í¼¡ï¸ Thermal & Power"
echo ""
echo "| Sensor | Temperature |"
echo "| :--- | :--- |"
for thermal in /sys/class/thermal/thermal_zone*; do
    TYPE=$(cat $thermal/type 2>/dev/null)
    TEMP_RAW=$(cat $thermal/temp 2>/dev/null)
    if [ -n "$TEMP_RAW" ]; then
        TEMP_C=$(awk "BEGIN {printf \"%.2f\", $TEMP_RAW/1000}")
        echo "| $TYPE | ${TEMP_C} Â°C |"
    fi
done
echo ""

# Fan status (if exists)
if [ -d /sys/class/hwmon ]; then
    HWMON_FAN=false
    for hw in /sys/class/hwmon/hwmon*; do
        NAME=$(cat $hw/name 2>/dev/null)
        if [ -f "$hw/fan1_input" ]; then
            RPM=$(cat $hw/fan1_input 2>/dev/null)
            echo "- **Fan ($NAME):** $RPM RPM"
            HWMON_FAN=true
        fi
    done
    if [ "$HWMON_FAN" = false ]; then
        echo "- **Fan:** No PWM/Fan sensor detected or inactive."
    fi
fi
echo ""

# Power Supply (if ADC or PMIC exposed)
if [ -f /sys/class/power_supply/main-battery/capacity ]; then
    echo "### Battery/PMIC Status"
    echo '```text'
    cat /sys/class/power_supply/*/uevent 2>/dev/null | grep -E "POWER_SUPPLY_NAME|POWER_SUPPLY_STATUS|POWER_SUPPLY_VOLTAGE|POWER_SUPPLY_CURRENT"
    echo '```'
else
    echo "> â„¹ï¸ No battery/PMIC status exposed via standard power_supply class."
fi
echo ""

# 7. PCIe Devices (Specific to RK3588 PCIe controllers)
echo "## 7. í ½í´— PCIe Devices"
echo ""
echo '```text'
lspci -nn
echo '```'
echo ""

# Footer
echo "---"
echo "*Report generated by rk3588_info.sh script*"

} > "$OUTPUT_FILE"

echo -e "${GREEN}Report saved to: ${OUTPUT_FILE}${NC}"
echo "You can view it using: cat $OUTPUT_FILE"

