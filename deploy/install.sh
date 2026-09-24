#!/bin/bash
#
# Sub2API Installation Script (fork releases: lwying/sub2api)
# Sub2API 安装脚本（fork 发布：lwying/sub2api）
# Usage: curl -sSL https://raw.githubusercontent.com/lwying/sub2api/main/deploy/install.sh | bash
#
# Scope: this script installs and rolls back the BINARY/systemd deployment only.
# Docker/Compose deployments must update the container image; replacing a file
# inside a running container is not an image upgrade and this script must not be
# used to fake one.
#
# Only releases actually published by lwying/sub2api that carry the archive for
# THIS platform plus checksums.txt are installable. A release that is image-only,
# missing the platform archive, or missing/mismatching checksums aborts before
# anything under the install directory is modified (fail closed).
#

set -e

# Bash 4+ is required for associative arrays used by the localized message table.
# Keep this guard before any Bash 4-only syntax so older shells fail with a clear hint.
if [ -z "${BASH_VERSION:-}" ]; then
    echo "Error: This installer must be run with Bash 4.0 or later." >&2
    echo "Please install Bash 4+ and run it with that interpreter." >&2
    exit 1
fi

BASH_MAJOR_VERSION="${BASH_VERSION%%.*}"
if [ "$BASH_MAJOR_VERSION" -lt 4 ]; then
    echo "Error: Bash 4.0 or later is required. Current version: $BASH_VERSION" >&2
    echo "Please install Bash 4+ and retry with that interpreter." >&2
    exit 1
fi

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Configuration
# Release source is the fork. The in-app updater, the rollback entry points and
# this installer must all resolve releases from the same repository.
GITHUB_REPO="lwying/sub2api"
# Asset names come from .goreleaser.yaml and must stay in sync with
# backend/internal/releasecontract (ArchiveName / ChecksumsAssetName).
CHECKSUMS_ASSET_NAME="checksums.txt"
INSTALL_DIR="/opt/sub2api"
SERVICE_NAME="sub2api"
SERVICE_USER="sub2api"
CONFIG_DIR="/etc/sub2api"

# Server configuration (will be set by user)
SERVER_HOST="0.0.0.0"
SERVER_PORT="8080"

# Language (default: zh = Chinese)
LANG_CHOICE="zh"

# ============================================================
# Language strings / 语言字符串
# ============================================================

# Chinese strings
declare -A MSG_ZH=(
    # General
    ["info"]="信息"
    ["success"]="成功"
    ["warning"]="警告"
    ["error"]="错误"

    # Language selection
    ["select_lang"]="请选择语言 / Select language"
    ["lang_zh"]="中文"
    ["lang_en"]="English"
    ["enter_choice"]="请输入选择 (默认: 1)"

    # Installation
    ["install_title"]="Sub2API 安装脚本"
    ["run_as_root"]="请使用 root 权限运行 (使用 sudo)"
    ["detected_platform"]="检测到平台"
    ["unsupported_arch"]="不支持的架构"
    ["unsupported_os"]="不支持的操作系统"
    ["missing_deps"]="缺少依赖"
    ["install_deps_first"]="请先安装以下依赖"
    ["fetching_version"]="正在获取最新版本..."
    ["latest_version"]="最新版本"
    ["failed_get_version"]="获取最新版本失败"
    ["downloading"]="正在下载"
    ["download_failed"]="下载失败"
    ["verifying_checksum"]="正在校验文件..."
    ["checksum_verified"]="校验通过"
    ["checksum_failed"]="校验失败"
    ["checksum_not_found"]="无法验证校验和（checksums.txt 未找到）"
    ["checksum_too_large"]="校验文件异常大，拒绝安装"
    ["extracting"]="正在解压..."
    ["binary_installed"]="二进制文件已安装到"
    ["binary_staging_failed"]="无法在安装目录安全地创建临时文件，拒绝继续安装"
    ["binary_target_invalid"]="安装路径是一个目录而不是可执行文件，拒绝继续安装"
    ["binary_restore_failed"]="无法把暂存的原可执行文件放回安装路径，已将其保留在"
    ["binary_restore_hint"]="请手动移回（该路径上不能有其他内容）："
    ["binary_restore_blocked"]="安装路径上是一个目录，请先移走它再恢复，否则文件会被移进该目录"
    ["staging_preserved"]="暂存目录未删除（其中保留着原可执行文件）"
    ["service_restore_skipped"]="安装路径上没有可执行文件，未启动服务"
    ["user_exists"]="用户已存在"
    ["creating_user"]="正在创建系统用户"
    ["user_created"]="用户已创建"
    ["setting_up_dirs"]="正在设置目录..."
    ["dirs_configured"]="目录配置完成"
    ["installing_service"]="正在安装 systemd 服务..."
    ["service_installed"]="systemd 服务已安装"
    ["ready_for_setup"]="准备就绪，可以启动设置向导"

    # Completion
    ["install_complete"]="Sub2API 安装完成！"
    ["install_dir"]="安装目录"
    ["next_steps"]="后续步骤"
    ["step1_check_services"]="确保 PostgreSQL 和 Redis 正在运行："
    ["step2_start_service"]="启动 Sub2API 服务："
    ["step3_enable_autostart"]="设置开机自启："
    ["step4_open_wizard"]="在浏览器中打开设置向导："
    ["wizard_guide"]="设置向导将引导您完成："
    ["wizard_db"]="数据库配置"
    ["wizard_redis"]="Redis 配置"
    ["wizard_admin"]="管理员账号创建"
    ["useful_commands"]="常用命令"
    ["cmd_status"]="查看状态"
    ["cmd_logs"]="查看日志"
    ["cmd_restart"]="重启服务"
    ["cmd_stop"]="停止服务"

    # Upgrade
    ["upgrading"]="正在升级 Sub2API..."
    ["current_version"]="当前版本"
    ["stopping_service"]="正在停止服务..."
    ["backup_created"]="备份已创建"
    ["backup_failed"]="备份当前二进制文件失败，拒绝继续安装"
    ["version_stamp_failed"]="已安装的版本号未能记录，下次将按未知版本处理"
    ["starting_service"]="正在启动服务..."
    ["upgrade_complete"]="升级完成！"

    # Version install
    ["installing_version"]="正在安装指定版本"
    ["version_not_found"]="指定版本不存在"
    ["same_version"]="已经是该版本，无需操作"
    ["rollback_complete"]="版本回退完成！"
    ["install_version_complete"]="指定版本安装完成！"
    ["validating_version"]="正在验证版本..."
    ["available_versions"]="可用版本列表"
    ["fetching_versions"]="正在获取可用版本..."
    ["not_installed"]="Sub2API 尚未安装，请先执行全新安装"
    ["fresh_install_hint"]="用法"

    # Fork release verification / 发布与校验（fail closed）
    ["release_query_failed"]="无法查询该版本的发布信息（网络或仓库不可用），拒绝继续"
    ["release_not_found"]="该版本标签在 fork 仓库中不存在或不可用（无 Release 数据）"
    ["release_not_published"]="该标签不是已正式发布的 Release（草稿或预发布），拒绝安装"
    ["checksums_asset_missing"]="该 Release 未提供校验文件，无法验证资产，拒绝安装"
    ["platform_asset_missing"]="该 Release 未提供当前平台的二进制资产，拒绝安装"
    ["invalid_version_format"]="版本号不符合 fork 规范（需要 vX.Y.Z 三段数字且不含前导零）"
    ["latest_not_installable"]="最新 fork 版本在当前平台不可安装，拒绝安装"
    ["skipped_not_installable"]="已跳过：当前平台无可用二进制资产或缺少校验文件"
    ["skipped_not_fork_version"]="已跳过：标签不是 fork 规范的 vX.Y.Z 版本号"
    ["checksum_entry_invalid"]="校验文件中找不到唯一的校验行，拒绝安装"
    ["checksum_tool_missing"]="系统缺少 sha256sum/shasum，无法校验下载内容，拒绝安装"
    ["unsafe_archive_path"]="压缩包中存在不安全路径（绝对路径或 ..），拒绝解压"
    ["unsafe_archive_member"]="压缩包中存在符号链接/硬链接等非普通文件成员，拒绝解压"
    ["archive_list_failed"]="无法读取压缩包成员列表（归档损坏或不可读），拒绝安装"
    ["archive_member_extract_failed"]="无法提取压缩包成员，拒绝安装"
    ["binary_not_executable"]="提取出的 sub2api 不是本平台的可执行文件，拒绝安装"
    ["binary_missing_in_archive"]="压缩包中未找到 sub2api 可执行文件，拒绝安装"
    ["deploy_member_skipped"]="已跳过会覆盖已验证程序的 deploy 成员"
    ["deploy_member_unsafe"]="已跳过无法安全放置的 deploy 成员（安装目录中同名项是目录）"
    ["deploy_staging_failed"]="无法在安装目录旁创建私有暂存目录，压缩包中的 deploy 文件未安装"
    ["binary_only_scope"]="本脚本只管理二进制（systemd）安装"
    ["no_installable_versions"]="当前平台没有可安装的 fork Release"
    ["docker_image_managed"]="Docker/Compose 部署请更新镜像版本升级，替换容器内文件不算升级"

    # Uninstall
    ["uninstall_confirm"]="这将从系统中移除 Sub2API。"
    ["are_you_sure"]="确定要继续吗？(y/N)"
    ["uninstall_cancelled"]="卸载已取消"
    ["removing_files"]="正在移除文件..."
    ["removing_install_dir"]="正在移除安装目录..."
    ["removing_user"]="正在移除用户..."
    ["config_not_removed"]="配置目录未被移除"
    ["remove_manually"]="如不再需要，请手动删除"
    ["removing_install_lock"]="正在移除安装锁文件..."
    ["install_lock_removed"]="安装锁文件已移除，重新安装时将进入设置向导"
    ["purge_prompt"]="是否同时删除配置目录？这将清除所有配置和数据 [y/N]: "
    ["removing_config_dir"]="正在移除配置目录..."
    ["uninstall_complete"]="Sub2API 已卸载"

    # Help
    ["usage"]="用法"
    ["cmd_none"]="(无参数)"
    ["cmd_install"]="安装 Sub2API"
    ["cmd_upgrade"]="升级到最新版本"
    ["cmd_uninstall"]="卸载 Sub2API"
    ["cmd_install_version"]="安装/回退到指定版本"
    ["cmd_list_versions"]="列出可用版本"
    ["opt_version"]="指定要安装的版本号 (例如: v1.0.0)"

    # Server configuration
    ["server_config_title"]="服务器配置"
    ["server_config_desc"]="配置 Sub2API 服务监听地址"
    ["server_host_prompt"]="服务器监听地址"
    ["server_host_hint"]="0.0.0.0 表示监听所有网卡，127.0.0.1 仅本地访问"
    ["server_port_prompt"]="服务器端口"
    ["server_port_hint"]="建议使用 1024-65535 之间的端口"
    ["server_config_summary"]="服务器配置"
    ["invalid_port"]="无效端口号，请输入 1-65535 之间的数字"

    # Service management
    ["starting_service"]="正在启动服务..."
    ["service_started"]="服务已启动"
    ["service_start_failed"]="服务启动失败，请检查日志"
    ["service_restarted"]="服务已重启，正在运行新版本"
    ["service_restoring"]="安装失败，正在恢复原有服务..."
    ["service_restored"]="已恢复原有服务"
    ["service_restoring_new"]="安装失败，但二进制已被替换；正在启动新安装的二进制..."
    ["service_started_new_binary"]="已启动新安装的二进制（原有二进制未被恢复）"
    ["service_restore_failed"]="恢复服务失败，请手动执行: sudo systemctl start sub2api"
    ["service_not_restarted"]="程序文件已就位但服务未运行，请手动启动: sudo systemctl start sub2api"
    ["enabling_autostart"]="正在设置开机自启..."
    ["autostart_enabled"]="开机自启已启用"
    ["getting_public_ip"]="正在获取公网 IP..."
    ["public_ip_failed"]="无法获取公网 IP，使用本地 IP"
)

# English strings
declare -A MSG_EN=(
    # General
    ["info"]="INFO"
    ["success"]="SUCCESS"
    ["warning"]="WARNING"
    ["error"]="ERROR"

    # Language selection
    ["select_lang"]="请选择语言 / Select language"
    ["lang_zh"]="中文"
    ["lang_en"]="English"
    ["enter_choice"]="Enter your choice (default: 1)"

    # Installation
    ["install_title"]="Sub2API Installation Script"
    ["run_as_root"]="Please run as root (use sudo)"
    ["detected_platform"]="Detected platform"
    ["unsupported_arch"]="Unsupported architecture"
    ["unsupported_os"]="Unsupported OS"
    ["missing_deps"]="Missing dependencies"
    ["install_deps_first"]="Please install them first"
    ["fetching_version"]="Fetching latest version..."
    ["latest_version"]="Latest version"
    ["failed_get_version"]="Failed to get latest version"
    ["downloading"]="Downloading"
    ["download_failed"]="Download failed"
    ["verifying_checksum"]="Verifying checksum..."
    ["checksum_verified"]="Checksum verified"
    ["checksum_failed"]="Checksum verification failed"
    ["checksum_not_found"]="Could not verify checksum (checksums.txt not found)"
    ["checksum_too_large"]="The checksum file is implausibly large; refusing to install"
    ["extracting"]="Extracting..."
    ["binary_installed"]="Binary installed to"
    ["binary_staging_failed"]="Could not create a staging file in the install directory; refusing to continue"
    ["binary_target_invalid"]="The install path is a directory rather than the executable; refusing to continue"
    ["binary_restore_failed"]="Could not put the parked installed executable back; it is preserved at"
    ["binary_restore_hint"]="Move it back by hand, with nothing else occupying that path:"
    ["binary_restore_blocked"]="A directory occupies the install path; move it away before restoring, or the executable would be moved inside it"
    ["staging_preserved"]="Staging directory kept (it holds the installed executable)"
    ["service_restore_skipped"]="The service was not started: there is no executable at the install path"
    ["user_exists"]="User already exists"
    ["creating_user"]="Creating system user"
    ["user_created"]="User created"
    ["setting_up_dirs"]="Setting up directories..."
    ["dirs_configured"]="Directories configured"
    ["installing_service"]="Installing systemd service..."
    ["service_installed"]="Systemd service installed"
    ["ready_for_setup"]="Ready for Setup Wizard"

    # Completion
    ["install_complete"]="Sub2API installation completed!"
    ["install_dir"]="Installation directory"
    ["next_steps"]="NEXT STEPS"
    ["step1_check_services"]="Make sure PostgreSQL and Redis are running:"
    ["step2_start_service"]="Start Sub2API service:"
    ["step3_enable_autostart"]="Enable auto-start on boot:"
    ["step4_open_wizard"]="Open the Setup Wizard in your browser:"
    ["wizard_guide"]="The Setup Wizard will guide you through:"
    ["wizard_db"]="Database configuration"
    ["wizard_redis"]="Redis configuration"
    ["wizard_admin"]="Admin account creation"
    ["useful_commands"]="USEFUL COMMANDS"
    ["cmd_status"]="Check status"
    ["cmd_logs"]="View logs"
    ["cmd_restart"]="Restart"
    ["cmd_stop"]="Stop"

    # Upgrade
    ["upgrading"]="Upgrading Sub2API..."
    ["current_version"]="Current version"
    ["stopping_service"]="Stopping service..."
    ["backup_created"]="Backup created"
    ["backup_failed"]="Failed to back up the current binary; refusing to continue"
    ["version_stamp_failed"]="Could not record the installed version; the next run will report it as unknown"
    ["starting_service"]="Starting service..."
    ["upgrade_complete"]="Upgrade completed!"

    # Version install
    ["installing_version"]="Installing specified version"
    ["version_not_found"]="Specified version not found"
    ["same_version"]="Already at this version, no action needed"
    ["rollback_complete"]="Version rollback completed!"
    ["install_version_complete"]="Specified version installed!"
    ["validating_version"]="Validating version..."
    ["available_versions"]="Available versions"
    ["fetching_versions"]="Fetching available versions..."
    ["not_installed"]="Sub2API is not installed. Please run a fresh install first"
    ["fresh_install_hint"]="Usage"

    # Fork release verification (fail closed)
    ["release_query_failed"]="Could not query this release (network or repository unavailable); refusing to continue"
    ["release_not_found"]="This version tag does not exist in the fork repository (no release data)"
    ["release_not_published"]="This tag is not a published release (draft or prerelease); refusing to install"
    ["checksums_asset_missing"]="This release does not publish a checksum file, so its assets cannot be verified; refusing to install"
    ["platform_asset_missing"]="This release does not publish a binary asset for the current platform; refusing to install"
    ["invalid_version_format"]="Version is not a fork release tag (need vX.Y.Z, three numeric segments, no leading zeros)"
    ["latest_not_installable"]="The latest fork release is not installable on this platform; refusing to install"
    ["skipped_not_installable"]="skipped: no binary asset for this platform, or checksum file missing"
    ["skipped_not_fork_version"]="skipped: tag is not a fork vX.Y.Z version"
    ["checksum_entry_invalid"]="No unique checksum line for this archive; refusing to install"
    ["checksum_tool_missing"]="sha256sum/shasum is missing, so the download cannot be verified; refusing to install"
    ["unsafe_archive_path"]="Archive contains an unsafe path (absolute or ..); refusing to extract"
    ["unsafe_archive_member"]="Archive contains a symlink, hard link or other non-regular member; refusing to extract"
    ["archive_list_failed"]="Could not read the archive member list (corrupt or unreadable archive); refusing to install"
    ["archive_member_extract_failed"]="Could not extract an archive member; refusing to install"
    ["binary_not_executable"]="The extracted sub2api is not an executable image for this platform; refusing to install"
    ["binary_missing_in_archive"]="Archive does not contain the sub2api executable; refusing to install"
    ["deploy_member_skipped"]="skipped a deploy member that would overwrite the verified binary"
    ["deploy_member_unsafe"]="skipped a deploy member that could not be placed safely (its name is a directory at the install path)"
    ["deploy_staging_failed"]="Could not create the private staging directory beside the install directory; the archive's deploy files were not installed"
    ["binary_only_scope"]="This script manages the binary (systemd) deployment only"
    ["no_installable_versions"]="No installable fork release for the current platform"
    ["docker_image_managed"]="Docker/Compose deployments upgrade by updating the image; replacing a file in the container is not an upgrade"

    # Uninstall
    ["uninstall_confirm"]="This will remove Sub2API from your system."
    ["are_you_sure"]="Are you sure? (y/N)"
    ["uninstall_cancelled"]="Uninstall cancelled"
    ["removing_files"]="Removing files..."
    ["removing_install_dir"]="Removing installation directory..."
    ["removing_user"]="Removing user..."
    ["config_not_removed"]="Config directory was NOT removed."
    ["remove_manually"]="Remove it manually if you no longer need it."
    ["removing_install_lock"]="Removing install lock file..."
    ["install_lock_removed"]="Install lock removed. Setup wizard will appear on next install."
    ["purge_prompt"]="Also remove config directory? This will delete all config and data [y/N]: "
    ["removing_config_dir"]="Removing config directory..."
    ["uninstall_complete"]="Sub2API has been uninstalled"

    # Help
    ["usage"]="Usage"
    ["cmd_none"]="(none)"
    ["cmd_install"]="Install Sub2API"
    ["cmd_upgrade"]="Upgrade to the latest version"
    ["cmd_uninstall"]="Remove Sub2API"
    ["cmd_install_version"]="Install/rollback to a specific version"
    ["cmd_list_versions"]="List available versions"
    ["opt_version"]="Specify version to install (e.g., v1.0.0)"

    # Server configuration
    ["server_config_title"]="Server Configuration"
    ["server_config_desc"]="Configure Sub2API server listen address"
    ["server_host_prompt"]="Server listen address"
    ["server_host_hint"]="0.0.0.0 listens on all interfaces, 127.0.0.1 for local only"
    ["server_port_prompt"]="Server port"
    ["server_port_hint"]="Recommended range: 1024-65535"
    ["server_config_summary"]="Server configuration"
    ["invalid_port"]="Invalid port number, please enter a number between 1-65535"

    # Service management
    ["starting_service"]="Starting service..."
    ["service_started"]="Service started"
    ["service_start_failed"]="Service failed to start, please check logs"
    ["service_restarted"]="Service restarted; the new version is now running"
    ["service_restoring"]="Install failed; restoring the previous service..."
    ["service_restored"]="Previous service restored"
    ["service_restoring_new"]="Install failed after the binary was replaced; starting the newly installed binary..."
    ["service_started_new_binary"]="Started the newly installed binary (the previous binary was not restored)"
    ["service_restore_failed"]="Could not restore the service; run: sudo systemctl start sub2api"
    ["service_not_restarted"]="The binary is in place but the service is not running; run: sudo systemctl start sub2api"
    ["enabling_autostart"]="Enabling auto-start on boot..."
    ["autostart_enabled"]="Auto-start enabled"
    ["getting_public_ip"]="Getting public IP..."
    ["public_ip_failed"]="Failed to get public IP, using local IP"
)

# Get message based on current language
msg() {
    local key="$1"
    if [ "$LANG_CHOICE" = "en" ]; then
        echo "${MSG_EN[$key]}"
    else
        echo "${MSG_ZH[$key]}"
    fi
}

# Print functions
print_info() {
    echo -e "${BLUE}[$(msg 'info')]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[$(msg 'success')]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[$(msg 'warning')]${NC} $1"
}

print_error() {
    echo -e "${RED}[$(msg 'error')]${NC} $1"
}

# Check if running interactively (can access terminal)
# When piped (curl | bash), stdin is not a terminal, but /dev/tty may still be available
is_interactive() {
    # Check if /dev/tty is available (works even when piped)
    [ -e /dev/tty ] && [ -r /dev/tty ] && [ -w /dev/tty ]
}

# Set while a running service has been stopped so the swap can happen. A failed
# verification uses it to bring the previous (still installed) binary back up:
# failing closed must not leave the host without a running service.
SERVICE_STOPPED_FOR_INSTALL=""

# Start the service after a failed install, and report what was really started.
#
# Which binary the service comes up around depends on whether the swap already
# happened (SWAP_COMPLETED): once the replacement is installed, the service that
# starts runs the NEW binary, so reporting "previous service restored" would be
# false. Shared by restore_service_if_stopped (explicit abort) and the EXIT trap
# (abort through set -e), so both report the same thing.
start_service_for_failed_install() {
    if [ "$SWAP_COMPLETED" = "true" ]; then
        print_warning "$(msg 'service_restoring_new')"
    else
        print_warning "$(msg 'service_restoring')"
    fi
    if systemctl start sub2api; then
        if [ "$SWAP_COMPLETED" = "true" ]; then
            print_success "$(msg 'service_started_new_binary')"
        else
            print_success "$(msg 'service_restored')"
        fi
    else
        print_error "$(msg 'service_restore_failed')"
    fi
}

restore_service_if_stopped() {
    [ "$SERVICE_STOPPED_FOR_INSTALL" = "true" ] || return 0
    SERVICE_STOPPED_FOR_INSTALL=""
    # The executable goes back before the service does: a start against an install
    # path without it (the entry is parked, see park_installed_executable) fails or
    # comes up around something that is not the installed executable. When it cannot
    # be put back, that path is not startable, so nothing is started and no restored
    # service is reported - saying so would be false.
    if [ -n "$PRESERVE_STAGE_DIR" ] || ! restore_parked_original; then
        print_error "$(msg 'service_restore_skipped')"
        return 0
    fi
    start_service_for_failed_install
}

# Abort an install attempt after a verification failure.
fail_install() {
    restore_service_if_stopped
    exit 1
}

# Single owner of the EXIT trap: remove the temp directory and, when a running
# service was stopped for a swap that never completed, start it again.
#
# Individual functions must not install their own EXIT trap: replacing this one
# would drop the service restore, so a failure that aborts through `set -e` (a
# failing cp, chown or tar with no explicit error branch) would leave the host
# without a running service.
TEMP_DIR=""

# Set by the entry points that replace an existing installation (upgrade,
# install_version) to the path the currently installed binary must be copied to
# before it is swapped out. download_and_extract performs that copy as the last
# step before the swap, so a release that fails any verification cannot destroy
# the rollback backup the operator already had.
BACKUP_PATH=""

# Set by download_and_extract to the sha256 of the verified executable it prepared
# to install, and read by write_version_stamp (called after the swap) to record the
# value get_current_version later checks the installed executable against.
#
# It is taken from the root-owned staged copy of that executable, before the rename
# that installs it, and not from the install path after the swap: the staged file is
# exactly the content the rename installs, and the service account cannot write that
# path, so the recorded hash cannot end up describing a binary that account
# substituted around the swap.
INSTALLED_BINARY_SHA256=""

# Staging directory for files that are about to be swapped into INSTALL_DIR. It is
# created by download_and_extract next to INSTALL_DIR (same filesystem, so the swap
# is a rename) and owned by root with mode 0700.
#
# It must not live inside INSTALL_DIR: that directory belongs to the service
# account (the account the service runs as, and the one the in-app updater writes
# its own binary from), so a file created there can be swapped for a symlink by
# that account between its creation and the root copy into it, however
# unpredictable its name - the copy would then write through the link, and a later
# rename would install the link. A root-owned directory keeps that account out of
# the staging path entirely. The EXIT trap removes it when an install aborts
# between staging and the swap.
STAGE_DIR=""

# Set by park_installed_executable to the path inside STAGE_DIR the installed
# executable was moved to by rename, while the swap is in progress. download_and_extract
# clears it once the replacement is in place (the rollback copy of it was taken by
# then); every abort path, including the EXIT trap, puts it back instead.
PARKED_ORIGINAL=""

# Set when a parked executable could not be put back. The staging directory that
# holds it must then survive cleanup: it is the only copy of the installed binary.
PRESERVE_STAGE_DIR=""

# Set by download_and_extract once the verified replacement is installed at the
# install path - the point where the parked original stops being this run's
# rollback source (PARKED_ORIGINAL is cleared there as well). An abort after that
# point leaves the NEW binary installed, so a service the EXIT trap starts runs it
# and must not be reported as the previous service restored.
SWAP_COMPLETED=""

# Put the parked installed executable back at its install path.
#
# Idempotent, and called by every abort path - including the EXIT trap for an abort
# that goes through set -e. Returns non-zero when the entry could not be put back.
restore_parked_original() {
    [ -n "$PARKED_ORIGINAL" ] || return 0

    # A directory (or a link to one) at the path would not be replaced by `mv`: the
    # parked executable would be moved *into* it, the path would stay a directory,
    # and the file would look restored while the executable is nested somewhere
    # else. That is not a restore, so it is not attempted.
    if [ ! -d "$INSTALL_DIR/sub2api" ]; then
        if mv -f "$PARKED_ORIGINAL" "$INSTALL_DIR/sub2api" 2>/dev/null; then
            PARKED_ORIGINAL=""
            return 0
        fi
    fi

    # Keep the file and name it: deleting the staging directory below would delete
    # the only copy of the installed binary, and a silently emptied install path
    # would leave the operator nothing to move back.
    if [ -z "$PRESERVE_STAGE_DIR" ]; then
        PRESERVE_STAGE_DIR="true"
        print_error "$(msg 'binary_restore_failed'): $PARKED_ORIGINAL"
        # The hint may not be a command that can quietly restore nothing: with a
        # directory at the install path, `mv -f parked INSTALL_DIR/sub2api` moves the
        # parked executable *into* that directory and leaves the path a directory, so
        # an operator following it verbatim would report a restore that restored
        # nothing. The blocking entry is therefore named, and the command is only
        # offered together with the precondition that nothing occupies the path.
        if [ -d "$INSTALL_DIR/sub2api" ]; then
            print_warning "$(msg 'binary_restore_blocked'): $INSTALL_DIR/sub2api"
        fi
        print_warning "$(msg 'binary_restore_hint') mv -f '$PARKED_ORIGINAL' '$INSTALL_DIR/sub2api'"
    fi
    return 1
}

cleanup_on_exit() {
    local status=$?

    # A failing cleanup must not abort the trap before the service restore below.
    if [ -n "$TEMP_DIR" ]; then
        rm -rf "$TEMP_DIR" 2>/dev/null || true
        TEMP_DIR=""
    fi

    # The parked executable goes back before the staging directory is removed: it is
    # this run's copy of the installed binary, and removing the directory while it
    # is still parked would remove the binary with it.
    restore_parked_original || true

    if [ -n "$STAGE_DIR" ]; then
        if [ "$PRESERVE_STAGE_DIR" = "true" ]; then
            # It holds the parked executable (see restore_parked_original), so it
            # survives this run.
            print_warning "$(msg 'staging_preserved'): $STAGE_DIR"
        else
            rm -rf "$STAGE_DIR" 2>/dev/null || true
            STAGE_DIR=""
        fi
    fi

    if [ "$SERVICE_STOPPED_FOR_INSTALL" = "true" ]; then
        SERVICE_STOPPED_FOR_INSTALL=""
        if [ -n "$PRESERVE_STAGE_DIR" ]; then
            # The parked executable is not at the install path (see above), so a
            # start would come up around nothing: it is not attempted, and a
            # restored service is not reported.
            print_error "$(msg 'service_restore_skipped')"
        else
            start_service_for_failed_install
        fi
    fi

    exit "$status"
}

trap cleanup_on_exit EXIT

# Select language
select_language() {
    # If not interactive (piped), use default language
    if ! is_interactive; then
        LANG_CHOICE="zh"
        return
    fi

    echo ""
    echo -e "${CYAN}=============================================="
    echo "  $(msg 'select_lang')"
    echo "==============================================${NC}"
    echo ""
    echo "  1) $(msg 'lang_zh') (默认/default)"
    echo "  2) $(msg 'lang_en')"
    echo ""

    read -p "$(msg 'enter_choice'): " lang_input < /dev/tty

    case "$lang_input" in
        2|en|EN|english|English)
            LANG_CHOICE="en"
            ;;
        *)
            LANG_CHOICE="zh"
            ;;
    esac

    echo ""
}

# Validate port number
validate_port() {
    local port="$1"
    if [[ "$port" =~ ^[0-9]+$ ]] && [ "$port" -ge 1 ] && [ "$port" -le 65535 ]; then
        return 0
    fi
    return 1
}

# Configure server settings
configure_server() {
    # If not interactive (piped), use default settings
    if ! is_interactive; then
        print_info "$(msg 'server_config_summary'): ${SERVER_HOST}:${SERVER_PORT} (default)"
        return
    fi

    echo ""
    echo -e "${CYAN}=============================================="
    echo "  $(msg 'server_config_title')"
    echo "==============================================${NC}"
    echo ""
    echo -e "${BLUE}$(msg 'server_config_desc')${NC}"
    echo ""

    # Server host
    echo -e "${YELLOW}$(msg 'server_host_hint')${NC}"
    read -p "$(msg 'server_host_prompt') [${SERVER_HOST}]: " input_host < /dev/tty
    if [ -n "$input_host" ]; then
        SERVER_HOST="$input_host"
    fi

    echo ""

    # Server port
    echo -e "${YELLOW}$(msg 'server_port_hint')${NC}"
    while true; do
        read -p "$(msg 'server_port_prompt') [${SERVER_PORT}]: " input_port < /dev/tty
        if [ -z "$input_port" ]; then
            # Use default
            break
        elif validate_port "$input_port"; then
            SERVER_PORT="$input_port"
            break
        else
            print_error "$(msg 'invalid_port')"
        fi
    done

    echo ""
    print_info "$(msg 'server_config_summary'): ${SERVER_HOST}:${SERVER_PORT}"
    echo ""
}

# Check if running as root
check_root() {
    # Use 'id -u' instead of $EUID for better compatibility
    # $EUID may not work reliably when script is piped to bash
    if [ "$(id -u)" -ne 0 ]; then
        print_error "$(msg 'run_as_root')"
        exit 1
    fi
}

# Detect OS and architecture
detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    case "$ARCH" in
        x86_64)
            ARCH="amd64"
            ;;
        aarch64|arm64)
            ARCH="arm64"
            ;;
        *)
            print_error "$(msg 'unsupported_arch'): $ARCH"
            exit 1
            ;;
    esac

    case "$OS" in
        linux)
            OS="linux"
            ;;
        darwin)
            OS="darwin"
            ;;
        *)
            print_error "$(msg 'unsupported_os'): $OS"
            exit 1
            ;;
    esac

    print_info "$(msg 'detected_platform'): ${OS}_${ARCH}"
}

# Check dependencies
check_dependencies() {
    local missing=()

    if ! command -v curl &> /dev/null; then
        missing+=("curl")
    fi

    if ! command -v tar &> /dev/null; then
        missing+=("tar")
    fi

    # Asset verification and archive inspection both parse tool output with awk.
    if ! command -v awk &> /dev/null; then
        missing+=("awk")
    fi

    # The installed binary's image magic is checked with od.
    if ! command -v od &> /dev/null; then
        missing+=("od (coreutils)")
    fi

    # Verification is mandatory, so a missing hash tool is a hard dependency
    # failure instead of a reason to skip the checksum.
    if ! command -v sha256sum &> /dev/null && ! command -v shasum &> /dev/null; then
        missing+=("sha256sum (coreutils) or shasum (perl)")
    fi

    if [ ${#missing[@]} -gt 0 ]; then
        print_error "$(msg 'missing_deps'): ${missing[*]}"
        print_info "$(msg 'install_deps_first')"
        exit 1
    fi
}

# Authenticate only GitHub REST API requests. Release asset downloads must stay anonymous.
github_api_curl() {
    local arg
    local expect_value=false
    local url

    if [ "$#" -lt 1 ]; then
        echo "github_api_curl requires exactly one GitHub API URL" >&2
        return 2
    fi
    url="${!#}"

    # Keep authenticated invocations constrained to the options used below. In
    # particular, curl config, --url, and --next could add another destination.
    for arg in "${@:1:$#-1}"; do
        if [ "$expect_value" = true ]; then
            expect_value=false
            continue
        fi
        case "$arg" in
            -s|--silent)
                ;;
            --connect-timeout|--max-time|-o|--output|-w|--write-out)
                expect_value=true
                ;;
            *)
                echo "Unsafe github_api_curl argument: $arg" >&2
                return 2
                ;;
        esac
    done

    if [ "$expect_value" = true ] || [[ "$url" != https://api.github.com/* ]]; then
        echo "github_api_curl requires exactly one GitHub API URL" >&2
        return 2
    fi

    if [ -n "${UPDATE_GITHUB_TOKEN:-}" ]; then
        if [[ "$UPDATE_GITHUB_TOKEN" == *$'\n'* || "$UPDATE_GITHUB_TOKEN" == *$'\r'* || "$UPDATE_GITHUB_TOKEN" == *'"'* || "$UPDATE_GITHUB_TOKEN" == *'\'* ]]; then
            echo "UPDATE_GITHUB_TOKEN contains unsupported characters" >&2
            return 2
        fi
        printf 'header = "Authorization: Bearer %s"\n' "$UPDATE_GITHUB_TOKEN" | UPDATE_GITHUB_TOKEN= GITHUB_TOKEN= GH_TOKEN= curl -q --globoff --config - "$@"
    else
        UPDATE_GITHUB_TOKEN= GITHUB_TOKEN= GH_TOKEN= curl -q --globoff "$@"
    fi
}

# Strict fork release tag: "vX.Y.Z" with three numeric segments and no leading
# zero in any segment. Mirrors backend/internal/releasecontract.ParseTag so the
# installer and the in-app updater accept exactly the same version inputs; a tag
# like "v0.2.7-rc.1" or "v01.2.3" is not a fork release.
is_strict_release_tag() {
    local tag="$1"
    [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
    if [[ "${tag#v}" =~ (^|\.)0[0-9] ]]; then
        return 1
    fi
    return 0
}

# Archive name for the running platform, matching .goreleaser.yaml and
# backend/internal/releasecontract.ArchiveName (.zip on Windows, .tar.gz
# elsewhere).
platform_archive_name() {
    local version_num="${1#v}"
    case "$OS" in
        windows)
            echo "sub2api_${version_num}_${OS}_${ARCH}.zip"
            ;;
        *)
            echo "sub2api_${version_num}_${OS}_${ARCH}.tar.gz"
            ;;
    esac
}

# GitHub answers an unknown tag with a 404 body rather than a transport error, so
# "no such tag" has to be recognised in the payload. A release object always
# carries a tag_name; a payload without one is not a usable release (unknown
# tag, error body, or truncated response) and must not be treated as installable.
release_json_has_tag() {
    printf '%s' "$1" | tr -d ' \t' | grep -Fq '"tag_name":'
}

# Whether the release JSON marks the release as published (not draft, not
# prerelease). A prerelease tagged vX.Y.Z must not silently become the install
# target, because the in-app updater compares numeric segments only.
release_json_is_published() {
    local json
    json=$(printf '%s' "$1" | tr -d ' \t')
    [[ "$json" == *'"draft":false'* && "$json" == *'"prerelease":false'* ]]
}

# Whether the release JSON lists this exact asset name in its assets array.
# Whitespace is stripped first so the comparison is an exact string match rather
# than a pattern.
#
# The search is confined to the part of the payload from "assets":[ onwards
# because "name" is not unique to assets: the release itself, its author and its
# uploaders all carry one, and any of them could happen to read exactly like an
# asset this gate is asked about. Matching those would make a release that
# publishes no archive (or no checksums.txt) look installable. The cut is sound
# because quotes inside a JSON string value are escaped, so an unescaped
# "name":"<asset>" key can only come from a real object - and after the assets
# array the release object holds no object that carries "name" again.
release_json_has_asset() {
    local json
    json=$(printf '%s' "$1" | tr -d ' \t')
    [[ "$json" == *'"assets":['* ]] || return 1
    json="${json#*\"assets\":\[}"
    printf '%s' "$json" | grep -Fq "\"name\":\"$2\""
}

# Single GitHub API call site for one release (kept as its own line so every
# api.github.com request still goes through the scoped authenticated helper).
fetch_release_json() {
    local tag="$1"
    github_api_curl -s --connect-timeout 10 --max-time 30 "https://api.github.com/repos/${GITHUB_REPO}/releases/tags/${tag}"
}

# Decide whether one tag offers an installable binary for the current platform.
# Prints "<reason_code>" or "<reason_code>|<detail>" on stdout and returns
# non-zero when it does not; prints an empty line and returns 0 when it does.
#
# This is the single gate every install and rollback path goes through, and it
# runs before anything is downloaded. An image-only release (the
# .goreleaser.simple.yaml path) publishes neither archives nor checksums, so it
# lands on checksums_asset_missing and is never installable; a binary release
# missing this platform's archive lands on platform_asset_missing.
release_installability() {
    local tag="$1"
    local json archive

    if ! json=$(fetch_release_json "$tag" 2>/dev/null) || [ -z "$json" ]; then
        printf '%s\n' "release_query_failed"
        return 1
    fi
    if ! release_json_has_tag "$json"; then
        printf '%s\n' "release_not_found"
        return 1
    fi
    if ! release_json_is_published "$json"; then
        printf '%s\n' "release_not_published"
        return 1
    fi
    if ! release_json_has_asset "$json" "$CHECKSUMS_ASSET_NAME"; then
        printf '%s|%s\n' "checksums_asset_missing" "$CHECKSUMS_ASSET_NAME"
        return 1
    fi
    archive=$(platform_archive_name "$tag")
    if ! release_json_has_asset "$json" "$archive"; then
        printf '%s|%s\n' "platform_asset_missing" "$archive"
        return 1
    fi
    printf '\n'
    return 0
}

# Fail-closed gate used by every install/rollback entry point.
require_installable_release() {
    local tag="$1"
    local reason code detail

    if ! reason=$(release_installability "$tag"); then
        code="${reason%%|*}"
        detail="${reason#*|}"
        if [ "$code" = "$detail" ]; then
            print_error "$(msg "$code")" >&2
        else
            print_error "$(msg "$code"): $detail" >&2
        fi
        return 1
    fi
    return 0
}

# The installed file must be a native executable image for THIS platform, not a
# text file, a link target string, an unrelated file streamed by mistake, or an
# image built for a different operating system (a Mach-O must not be installable
# on Linux, and vice versa). Fork releases are Go binaries: ELF on Linux, Mach-O
# (thin or universal) on macOS.
#
# A missing or failing od yields an empty magic, which is refused: the check
# fails closed rather than skipping verification.
verify_binary_image() {
    local file="$1"
    local magic
    magic=$(od -An -v -tx1 -N4 "$file" 2>/dev/null | tr -d ' \n')
    case "$OS" in
        linux)
            case "$magic" in
                7f454c46) # \177ELF
                    return 0
                    ;;
            esac
            ;;
        darwin)
            case "$magic" in
                cffaedfe|feedfacf|cefaedfe|feedface|cafebabe|bebafeca) # Mach-O / fat
                    return 0
                    ;;
            esac
            ;;
    esac
    return 1
}

# Resolve to stdout the sha256 of a file, or fail when no hash tool exists (the
# caller must treat that as a failed verification, never as "skip").
sha256_of_file() {
    local file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    else
        return 1
    fi
}

# Get latest release version
#
# The latest fork release only becomes the install target when it is a strict
# vX.Y.Z tag AND actually offers this platform's archive plus checksums.txt.
# An image-only "latest" (or one missing the platform archive) aborts instead of
# being silently swapped for an older version.
get_latest_version() {
    local tag

    print_info "$(msg 'fetching_version')"
    # The payload is matched after whitespace is removed and the key/value pair is
    # located as a whole: the API is free to send the object on one line or
    # pretty-printed, and a line-based extraction of the last quoted string on the
    # line reads a neighbouring field (or a value from the payload) as the tag.
    tag=$(github_api_curl -s --connect-timeout 10 --max-time 30 "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" 2>/dev/null |
        tr -d ' \t' | grep -o '"tag_name":"[^"]*"' | sed -E 's/^"tag_name":"(.*)"$/\1/')

    if [ -z "$tag" ]; then
        print_error "$(msg 'failed_get_version')"
        print_info "Please check your network connection or try again later."
        exit 1
    fi

    if ! is_strict_release_tag "$tag"; then
        print_error "$(msg 'invalid_version_format'): $tag"
        exit 1
    fi

    if ! require_installable_release "$tag"; then
        print_error "$(msg 'latest_not_installable'): $tag"
        print_info "Use '$0 list-versions' to see fork releases installable on ${OS}_${ARCH}."
        exit 1
    fi

    LATEST_VERSION="$tag"
    print_info "$(msg 'latest_version'): $LATEST_VERSION"
}

# List available versions
#
# This is the human-facing rollback candidate list, so it only shows fork
# releases that are actually installable on this platform. Image-only releases
# and releases missing the platform archive or checksums.txt are reported as
# skipped rather than offered as a rollback target; the installer would refuse
# them anyway.
#
# The releases feed is newest-first and image-only builds sit in it alongside
# installable ones, so the candidate cap applies to the entries that are actually
# listed, not to the feed: truncating the feed first would hide an older release
# that is still a valid rollback target behind newer releases that are not.
list_versions() {
    local tags tag reason code detail listed=0 skipped=0
    local limit=20 per_page=100

    print_info "$(msg 'fetching_versions')"
    # Every tag_name in the payload is collected, one per line, whatever the
    # whitespace: the feed may arrive as one line or pretty-printed, and a
    # line-based extraction would read a neighbouring field as a tag and then
    # report it as a non-fork version instead of listing the real candidates.
    tags=$(github_api_curl -s --connect-timeout 10 --max-time 30 "https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=${per_page}" 2>/dev/null |
        tr -d ' \t' | grep -o '"tag_name":"[^"]*"' | sed -E 's/^"tag_name":"(.*)"$/\1/')

    if [ -z "$tags" ]; then
        print_error "$(msg 'failed_get_version')"
        print_info "Please check your network connection or try again later."
        exit 1
    fi

    echo ""
    echo "$(msg 'available_versions') (${OS}_${ARCH}):"
    echo "----------------------------------------"
    while read -r tag; do
        [ -n "$tag" ] || continue
        if ! is_strict_release_tag "$tag"; then
            print_warning "$(msg 'skipped_not_fork_version'): $tag"
            skipped=$((skipped + 1))
            continue
        fi
        if reason=$(release_installability "$tag"); then
            echo "  $tag"
            listed=$((listed + 1))
            if [ "$listed" -ge "$limit" ]; then
                break
            fi
        else
            code="${reason%%|*}"
            detail="${reason#*|}"
            if [ "$code" = "$detail" ]; then
                detail=""
            else
                detail=" ($detail)"
            fi
            print_warning "$(msg 'skipped_not_installable'): $tag$detail"
            skipped=$((skipped + 1))
        fi
    done <<< "$tags"
    echo "----------------------------------------"
    if [ "$listed" -eq 0 ]; then
        print_warning "$(msg 'no_installable_versions')"
    fi
    print_info "$(msg 'available_versions'): installable=$listed skipped=$skipped"
    echo ""
}

# Validate a requested version and make sure it is an installable fork release
#
# Two independent checks, both fail closed:
#   1. the input normalizes to a strict fork tag (vX.Y.Z, three numeric
#      segments, no leading zeros) rather than any v* tag that happens to exist;
#   2. the release actually publishes this platform's archive plus checksums.txt,
#      so an image-only or incomplete release can never become a rollback target.
# The normalized tag is printed to stdout.
validate_version() {
    local version="$1"

    # Check for empty version
    if [ -z "$version" ]; then
        print_error "$(msg 'opt_version')" >&2
        exit 1
    fi

    # Ensure version starts with 'v'
    if [[ ! "$version" =~ ^v ]]; then
        version="v$version"
    fi

    if ! is_strict_release_tag "$version"; then
        print_error "$(msg 'invalid_version_format'): $version" >&2
        echo "" >&2
        list_versions >&2
        exit 1
    fi

    print_info "$(msg 'validating_version') $version" >&2

    if ! require_installable_release "$version"; then
        print_error "$(msg 'version_not_found'): $version" >&2
        echo "" >&2
        list_versions >&2
        exit 1
    fi

    # Return the normalized version (to stdout)
    echo "$version"
}

# Path of the file that records the version of the installed executable.
#
# INSTALL_DIR belongs to the service account (setup_directories chowns it, and the
# in-app updater replaces the binary from there), so the file at
# $INSTALL_DIR/sub2api can be replaced by that account at any time. Asking the
# executable what version it is would run whatever that account left there as root:
# the service account would choose code the installer executes with root's
# privileges. The version is therefore recorded here instead of being read back by
# executing the binary.
#
# The stamp lives beside INSTALL_DIR, in the root-owned directory that already
# holds the staging directory, and not inside it: that is the one location the
# service account can write, so a version file there could be replaced - or swapped
# for a link - by the account the version is being reported about. It is not a
# secret and carries no privilege of its own: it is only ever a version label and a
# suffix of the rollback backup file name.
#
# A version alone is a claim about the executable that was installed, and that
# executable can be replaced by this account without the stamp being rewritten: the
# in-app updater swaps the binary in INSTALL_DIR itself and knows nothing about this
# file. A version read back from such a stamp would therefore describe the
# replacement, so the stamp also records the sha256 of the executable it was
# written for, and get_current_version reports a version only while that hash still
# matches the executable that is installed. Comparing the hash reads the file, it
# never runs it: the account still cannot get code executed as root this way.
version_stamp_path() {
    printf '%s\n' "$(dirname "$INSTALL_DIR")/.sub2api.version"
}

# Record the version of the release that was just installed, and the sha256 of the
# executable it was installed as, by writing both into the root-owned staging
# directory and renaming that file onto the stamp path. Returns non-zero when the
# stamp could not be recorded; the caller decides whether that is fatal (it is not:
# the install itself is complete and the version only stays unrecorded).
write_version_stamp() { # <version> <sha256-of-installed-executable>
    local version="$1" hash="$2" stamp staged

    # An empty version would record a stamp that get_current_version then has to
    # reject again, and a stamp without a hash could never be checked against the
    # executable it describes, so get_current_version would reject it as well: both
    # are a failed recording rather than a stamp worth writing. This writes through
    # the private, root-owned staging directory the caller prepared, which is still
    # in place at this point of the install.
    [ -n "$version" ] || return 1
    [ -n "$hash" ] || return 1
    [ -n "$STAGE_DIR" ] || return 1
    case "$version" in
        v*) ;;
        *) version="v$version" ;;
    esac

    stamp=$(version_stamp_path)
    staged="$STAGE_DIR/sub2api.version"

    if ! printf '%s\nsha256=%s\n' "$version" "$hash" > "$staged"; then
        rm -f "$staged"
        return 1
    fi
    # Cheap tripwire, as for the staged binary: the file about to be renamed must be
    # the regular file written just above, in the directory this run created.
    if [ ! -f "$staged" ] || [ -L "$staged" ]; then
        rm -f "$staged"
        return 1
    fi
    # A rename replaces the destination entry, it does not follow it, so a stamp
    # path holding something else is replaced rather than written through.
    if ! mv -f "$staged" "$stamp"; then
        rm -f "$staged"
        return 1
    fi
    [ -f "$stamp" ] || return 1
    return 0
}

# Get current installed version
#
# Never prints an empty version, and never executes the installed executable: see
# version_stamp_path above - the file at that path can have been left there by the
# service account, so running it as the installer (which needs root) would hand that
# account root. A host that was installed before this stamp existed reports
# "unknown" until the next release this installer installs records it.
#
# A version is reported only while the stamp still describes the executable that is
# installed: the sha256 recorded in it is compared with the sha256 of the file at
# the install path (see version_stamp_path). When the two differ, the executable was
# replaced outside this script - the in-app updater swaps the binary without
# rewriting the stamp - so the recorded version belongs to a binary that is no
# longer installed. Reporting it anyway is not a cosmetic mislabel:
# install_version reads this version to decide whether the requested release is
# already the one installed, so a rollback to the version the stamp still names
# would exit successfully while installing nothing, and the backup it takes on the
# way would be named after a version the binary being replaced is not. The version
# of a binary replaced out of band cannot be recovered without running it, which
# must never happen here, so it is reported as "unknown" - the same answer a host
# with no stamp gets, and the one that makes install_version install the requested
# release instead of skipping it.
#
# Reading the file to hash it is a read, not a run: nothing below executes the
# installed executable, and sha256_of_file is the hash tool, not the binary.
#
# That read is of a path the service account can write, so between the link check
# below and the hash it can point the path at a file of its choosing and have root
# read that file - a shell script cannot portably hash an already-opened file
# descriptor, and there is no way to park the entry here, because this lookup runs
# before the service is stopped. The read is accepted rather than papered over with a
# re-check that cannot be made atomic: the digest is only ever compared with the one
# recorded in the root-owned stamp and never printed, so the account gets no content
# back and no oracle either - the comparison's outcome is not reported to it - and
# every outcome other than an exact match is "unknown", the fail-closed direction for
# the decision this value feeds.
#
# The fallback cannot hang off the pipeline: `head` exits 0 with empty input, so a
# stamp that cannot be parsed would end the pipeline successfully and the `||` would
# never fire - leaving an empty string that would then be interpolated into the
# rollback backup file name.
get_current_version() {
    if [ ! -f "$INSTALL_DIR/sub2api" ]; then
        echo "not_installed"
        return 0
    fi

    local stamp version recorded_hash actual_hash
    stamp=$(version_stamp_path)
    version=""
    recorded_hash=""
    if [ -f "$stamp" ] && [ ! -L "$stamp" ]; then
        # Use grep -E for better compatibility (works on macOS and Linux)
        version=$(grep -oE 'v?[0-9]+\.[0-9]+\.[0-9]+' "$stamp" 2>/dev/null | head -1 || true)
        recorded_hash=$(grep -oE 'sha256=[0-9a-f]{64}' "$stamp" 2>/dev/null | head -1 || true)
        recorded_hash=${recorded_hash#sha256=}
    fi

    # The stamp is evidence about the installed executable only when it carries both
    # a version and the hash of that executable. A stamp without the hash (written
    # before the hash was recorded) cannot be checked against anything at all, which
    # is not the same as being correct: it is not evidence that the version it names
    # is still the one installed.
    if [ -z "$version" ] || [ -z "$recorded_hash" ]; then
        echo "unknown"
        return 0
    fi

    # The hash describes the entry this installer put at the install path. A link
    # there is an entry it did not install, and hashing it would read through it.
    if [ ! -f "$INSTALL_DIR/sub2api" ] || [ -L "$INSTALL_DIR/sub2api" ]; then
        echo "unknown"
        return 0
    fi

    # A missing hash tool or an unreadable file leaves the claim unproven, and an
    # unproven claim is reported as unknown rather than assumed correct.
    # check_dependencies requires the tool for every entry point that reaches here.
    actual_hash=$(sha256_of_file "$INSTALL_DIR/sub2api" 2>/dev/null) || actual_hash=""
    if [ "$actual_hash" != "$recorded_hash" ]; then
        echo "unknown"
        return 0
    fi

    printf '%s\n' "$version"
}

# Copy a file into the root-owned staging directory (STAGE_DIR) and print the
# staged path; the caller renames it over its destination, which is on the same
# filesystem, so the rename replaces the destination directory entry instead of
# following it.
#
# Nothing is ever written into INSTALL_DIR itself: that is the one location the
# service account can write to, so a file created there - however unpredictable its
# name - could be swapped for a symlink before the root copy opens it, and the copy
# would then write through the link. A root-owned staging directory is not
# reachable by that account at all.
stage_install_file() { # <source> <staged-name>
    local source="$1" name="$2" staged="$STAGE_DIR/$2"

    if ! cp "$source" "$staged"; then
        rm -f "$staged"
        return 1
    fi
    # Cheap tripwire: the copy must have landed in the root-owned directory this
    # run created, as a regular file.
    if [ ! -f "$staged" ] || [ -L "$staged" ] || [ -L "$STAGE_DIR" ]; then
        rm -f "$staged"
        return 1
    fi
    printf '%s\n' "$staged"
}

# Move the installed executable out of INSTALL_DIR and into the private staging
# directory by rename (PARKED_ORIGINAL), and return non-zero without parking
# anything when it cannot.
#
# INSTALL_DIR is writable by the service account: the in-app updater replaces the
# binary from there. Any path inside it can therefore be swapped for a link between
# a check and a read of it, so a check placed immediately before the read - however
# small the window - cannot close that. A rename moves the directory entry itself:
# a planted link is parked as a link and nothing is read through it, the parked
# entry is validated out of that account's reach, and it is the path the backup is
# read from. It is put back by every abort path (restore_parked_original).
#
# The parked path is recorded in PARKED_ORIGINAL, not printed: the caller reads the
# global, because a command substitution would run this in a subshell and the abort
# paths in the parent would never see the marker.
park_installed_executable() { # <parked-name>
    local name="$1" inode_before inode_after
    local parked="$STAGE_DIR/$1"

    # `mv` moves an operand *into* an existing directory instead of replacing it, so
    # an entry at the destination would park the executable under a nested name.
    if [ -e "$parked" ] || [ -L "$parked" ]; then
        return 1
    fi
    # The inode of the entry that is about to be parked. Comparing it afterwards
    # proves the park really was a rename: a rename keeps the inode, while the copy
    # `mv` falls back to when the staging directory is not on the same filesystem
    # does not - and that copy reads the entry, the read this function exists to
    # avoid. A mismatch aborts the install instead of trusting the copy.
    inode_before=$(ls -di "$INSTALL_DIR/sub2api" 2>/dev/null | awk '{print $1}') || return 1
    if [ -z "$inode_before" ]; then
        return 1
    fi
    if ! mv -f "$INSTALL_DIR/sub2api" "$parked"; then
        return 1
    fi
    # The entry is out of INSTALL_DIR now, so record it before anything else can
    # abort: an abort that skipped this would leave the staging directory holding
    # the only copy of the installed binary, for cleanup to delete.
    PARKED_ORIGINAL="$parked"

    inode_after=$(ls -di "$parked" 2>/dev/null | awk '{print $1}') || return 1
    if [ "$inode_before" != "$inode_after" ]; then
        return 1
    fi
    # Only a regular file is the installed executable: a link, directory, fifo or
    # device parked here must never become the rollback source.
    if [ ! -f "$parked" ] || [ -L "$parked" ]; then
        return 1
    fi
    return 0
}

# Point BACKUP_PATH at where the installed binary must be kept when an entry point
# replaces an existing installation without naming a version ("install" and the
# default command both reinstall the latest release over whatever is installed).
#
# download_and_extract swaps that executable out by rename, so without a backup
# path the binary it replaces is simply gone: the operator loses the last
# known-good build with no rollback point. The copy is taken there after the
# release has passed verification and immediately before the swap, so a release
# that never installs still leaves the previous rollback copy in place.
#
# A fresh install (no executable at the install path) leaves BACKUP_PATH unset:
# there is nothing to back up, and parking a missing entry aborts the install.
backup_path_for_existing_install() {
    if [ -f "$INSTALL_DIR/sub2api" ]; then
        BACKUP_PATH="$INSTALL_DIR/sub2api.backup"
    fi
}

# Download, verify and extract a release into INSTALL_DIR
#
# Ordering is deliberate: everything that can fail happens in a temp directory
# first, and only a verified archive with a real sub2api executable inside is
# allowed to touch INSTALL_DIR. A missing checksums.txt, an ambiguous or
# mismatching checksum, an archive that tries to escape its extraction directory,
# or an archive without the binary all abort with INSTALL_DIR untouched, so the
# currently running executable and the operator's existing rollback path survive.
# A caller that replaces an existing installation points BACKUP_PATH at where the
# binary being replaced must be kept; that copy is taken here, after verification
# and immediately before the swap, so only an installation that really happens
# replaces the previous rollback backup.
download_and_extract() {
    local archive_name
    archive_name=$(platform_archive_name "$LATEST_VERSION")
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/${LATEST_VERSION}/${archive_name}"
    local checksum_url="https://github.com/${GITHUB_REPO}/releases/download/${LATEST_VERSION}/${CHECKSUMS_ASSET_NAME}"
    local checksum_file expected_checksum actual_checksum entry
    local member member_path member_matches member_type
    local staged staged_backup
    local checksum_max_bytes=1048576
    local -a checksum_matches=()

    # Defense in depth: never download from a tag that is not an installable fork
    # release, even if a caller forgot to validate it first.
    if ! require_installable_release "$LATEST_VERSION"; then
        fail_install
    fi

    print_info "$(msg 'downloading') ${archive_name}..."

    # Create temp directory. Cleanup, and the service restore for a swap that
    # never completes, is owned by the single EXIT trap installed at the top of
    # this script, so nothing here replaces it.
    TEMP_DIR=$(mktemp -d)
    mkdir -p "$TEMP_DIR/extract"

    # Download archive (-f: an HTTP error must not land in a file we then unpack;
    # -q --globoff: a hostile curlrc must not be able to add options or rewrite
    # the destination of an asset download)
    if ! curl -q --globoff -sfL "$download_url" -o "$TEMP_DIR/$archive_name"; then
        print_error "$(msg 'download_failed')"
        fail_install
    fi

    # The checksum file is required. Without it the archive cannot be shown to
    # match what the release published, and the download host alone does not
    # prove provenance, so installation stops here.
    print_info "$(msg 'verifying_checksum')"
    checksum_file="$TEMP_DIR/$CHECKSUMS_ASSET_NAME"
    # The checksum file holds one line per published asset, so it is a few KiB at
    # most. --max-filesize stops a server from streaming an endless body into the
    # temp directory (curl fails as soon as the response exceeds the limit), and
    # the size is checked again below because the limit is not enforced by every
    # curl version and protocol.
    if ! curl -q --globoff -sfL --max-filesize "$checksum_max_bytes" "$checksum_url" -o "$checksum_file"; then
        print_error "$(msg 'checksum_not_found')"
        print_error "Refusing to install ${archive_name} without ${CHECKSUMS_ASSET_NAME}."
        fail_install
    fi
    if [ "$(wc -c < "$checksum_file")" -gt "$checksum_max_bytes" ]; then
        print_error "$(msg 'checksum_too_large'): ${CHECKSUMS_ASSET_NAME}"
        fail_install
    fi

    # Require exactly one well-formed checksum line for this archive. Accept the
    # sha256sum text modes ("hash  name" and "hash *name") and an optional "./".
    mapfile -t checksum_matches < <(awk -v target="$archive_name" '
        {
            name = $2
            sub(/^\*/, "", name)
            sub(/^\.\//, "", name)
        }
        name == target { print $1 }
    ' "$checksum_file")
    if [ "${#checksum_matches[@]}" -ne 1 ]; then
        print_error "$(msg 'checksum_entry_invalid'): ${archive_name}"
        fail_install
    fi
    expected_checksum="${checksum_matches[0]}"
    if [[ ! "$expected_checksum" =~ ^[0-9a-fA-F]{64}$ ]]; then
        print_error "$(msg 'checksum_entry_invalid'): ${archive_name}"
        fail_install
    fi

    if ! actual_checksum=$(sha256_of_file "$TEMP_DIR/$archive_name"); then
        print_error "$(msg 'checksum_tool_missing')"
        fail_install
    fi

    if [ "$expected_checksum" != "$actual_checksum" ]; then
        print_error "$(msg 'checksum_failed')"
        print_error "Expected: $expected_checksum"
        print_error "Actual: $actual_checksum"
        fail_install
    fi
    print_success "$(msg 'checksum_verified')"

    # The member list and the member types are captured into files first, with
    # the exit status of each tar call checked explicitly: with a process
    # substitution a corrupt archive (or a tampered listing) would silently
    # produce an empty list and skip every check below.
    if ! tar -tzf "$TEMP_DIR/$archive_name" > "$TEMP_DIR/members" 2>/dev/null; then
        print_error "$(msg 'archive_list_failed')"
        fail_install
    fi
    if [ ! -s "$TEMP_DIR/members" ]; then
        print_error "$(msg 'archive_list_failed')"
        fail_install
    fi
    if ! tar -tvzf "$TEMP_DIR/$archive_name" > "$TEMP_DIR/members.verbose" 2>/dev/null; then
        print_error "$(msg 'archive_list_failed')"
        fail_install
    fi
    if [ ! -s "$TEMP_DIR/members.verbose" ]; then
        print_error "$(msg 'archive_list_failed')"
        fail_install
    fi

    # Member names must be relative, free of ".." components, and free of
    # pattern characters so that each member operand below addresses exactly one
    # member. Member types must be a regular file ("-") or a directory ("d").
    # The type is the first field of tar -tv output for both GNU and BSD tar and
    # anything else (symlink "l", hard link "h", device, fifo) is refused: a
    # symlink member can point outside the extraction root and a member written
    # later would then be created through it.
    while IFS= read -r entry; do
        case "$entry" in
            /*|..|../*|*/../*|*/..)
                print_error "$(msg 'unsafe_archive_path'): $entry"
                fail_install
                ;;
            *'*'*|*'?'*|*'['*|*']'*|*'\'*)
                print_error "$(msg 'unsafe_archive_member'): $entry"
                fail_install
                ;;
        esac
    done < "$TEMP_DIR/members"

    while IFS= read -r member_type _; do
        [ -n "$member_type" ] || continue
        case "${member_type:0:1}" in
            -|d) ;;
            *)
                print_error "$(msg 'unsafe_archive_member'): $member_type"
                fail_install
                ;;
        esac
    done < "$TEMP_DIR/members.verbose"

    # Extract by streaming one member at a time to stdout (-xO). tar writes
    # nothing to the filesystem here, so no member can be created through a link
    # pointing outside the install tree, and directory members are never
    # recursed into: every file below is written by this script itself.
    print_info "$(msg 'extracting')"
    mkdir -p "$TEMP_DIR/extract"
    while IFS= read -r member; do
        [ -n "$member" ] || continue
        # Depending on how the archive was built, members may be prefixed with
        # "./" (a "tar -C dir ." build does); the install layout is matched on
        # the path without that prefix.
        case "$member" in
            ./*) member_path="${member#./}" ;;
            *) member_path="$member" ;;
        esac
        case "$member_path" in
            */|'') continue ;;
            sub2api|deploy/*) ;;
            *) continue ;;
        esac
        # tar matches a member operand as a prefix (operand "deploy" also
        # selects "deploy/x"), so refuse an archive where streaming this member
        # would pull in additional members.
        member_matches=$(awk -v wanted="$member" 'index($0, wanted) == 1' "$TEMP_DIR/members" | wc -l)
        if [ "$member_matches" -ne 1 ]; then
            print_error "$(msg 'unsafe_archive_member'): $member"
            fail_install
        fi
        mkdir -p "$TEMP_DIR/extract/$(dirname "$member_path")"
        if ! tar -xOzf "$TEMP_DIR/$archive_name" -- "$member" > "$TEMP_DIR/extract/$member_path" 2>/dev/null; then
            print_error "$(msg 'archive_member_extract_failed'): $member"
            fail_install
        fi
    done < "$TEMP_DIR/members"

    # The installed binary must be a non-empty regular file, and it must really
    # be a native executable image: a link target or an unrelated file streamed
    # by mistake would otherwise be installed in its place.
    if [ ! -f "$TEMP_DIR/extract/sub2api" ] || [ -L "$TEMP_DIR/extract/sub2api" ] || [ ! -s "$TEMP_DIR/extract/sub2api" ]; then
        print_error "$(msg 'binary_missing_in_archive')"
        fail_install
    fi
    if ! verify_binary_image "$TEMP_DIR/extract/sub2api"; then
        print_error "$(msg 'binary_not_executable')"
        fail_install
    fi

    # Create install directory, and beside it the root-owned staging directory the
    # swap goes through. Same parent directory, so the renames below are renames on
    # one filesystem and cannot degrade into a copy.
    mkdir -p "$INSTALL_DIR"
    if ! STAGE_DIR=$(mktemp -d "$(dirname "$INSTALL_DIR")/.sub2api.stage.XXXXXXXX"); then
        print_error "$(msg 'binary_staging_failed')"
        fail_install
    fi

    # Prepare the replacement completely in the private staging directory first:
    # copy, mode and ownership. None of this touches INSTALL_DIR, so a failure here
    # leaves both the installed binary and the existing rollback backup exactly as
    # the operator had them, for an install that never happened.
    if ! staged=$(stage_install_file "$TEMP_DIR/extract/sub2api" "sub2api"); then
        print_error "$(msg 'binary_staging_failed')"
        fail_install
    fi
    chmod 755 "$staged"
    # Give the staged file to the service account here, before the swap: a chown
    # that fails afterwards would abort with the new binary already installed,
    # and the EXIT trap would then start a binary it failed to prepare while
    # reporting the previous service as restored. On a fresh install the account
    # does not exist yet and setup_directories sets the ownership instead.
    if id "$SERVICE_USER" &>/dev/null; then
        chown "$SERVICE_USER:$SERVICE_USER" "$staged"
    fi

    # Hash that prepared copy here, while it is still the root-owned staged file: the
    # rename below installs exactly these bytes, so this value describes the
    # executable that is about to be installed, and it is read from a path the
    # service account cannot write. Hashing the install path after the swap instead
    # would let that account win the moment between the rename and the hash and have
    # its own file recorded as the version this script installed (see
    # INSTALLED_BINARY_SHA256).
    #
    # A missing hash tool leaves the value empty; write_version_stamp refuses to
    # record a stamp without it, so the version is simply left unrecorded -
    # check_dependencies requires the tool for every entry point that reaches here.
    INSTALLED_BINARY_SHA256=$(sha256_of_file "$staged" 2>/dev/null) || INSTALLED_BINARY_SHA256=""

    # Back up the binary that is about to be replaced - and only now, with a
    # prepared replacement in hand: a release that failed verification, staging or
    # the ownership step above must not have destroyed the rollback backup of an
    # install that never happened. This is the last step before the swap.
    if [ -n "$BACKUP_PATH" ]; then
        # The installed executable is read here as root and the service account can
        # write that path, so a link planted there would make this copy read
        # whatever it points at (a root-only file) and end up as a backup inside
        # INSTALL_DIR, which that account can read.
        #
        # The entry is therefore parked into the private staging directory by rename
        # before it is read, and read from there: a rename moves an entry without
        # reading it, so a link planted in the meantime is parked as a link and
        # refused by the parked-entry validation, and the source of the copy is a
        # path that account cannot write at all. Checking the entry in INSTALL_DIR
        # and copying it in place could not close that window.
        if ! park_installed_executable "sub2api.parked"; then
            print_error "$(msg 'binary_target_invalid'): $INSTALL_DIR/sub2api"
            fail_install
        fi
        # PARKED_ORIGINAL, set by the park above: the parked entry is the rollback
        # source, and the path it was parked from is not read again.
        if ! staged_backup=$(stage_install_file "$PARKED_ORIGINAL" "sub2api.backup"); then
            print_error "$(msg 'backup_failed')"
            fail_install
        fi
        if [ -d "$BACKUP_PATH" ]; then
            print_error "$(msg 'backup_failed'): $BACKUP_PATH"
            fail_install
        fi
        mv -f "$staged_backup" "$BACKUP_PATH"
        # Fail closed if that rename did not land the backup: a directory planted at
        # the path after the check above receives the staged file instead of being
        # replaced by it, and the run would then report a rollback copy that does
        # not exist.
        if [ ! -f "$BACKUP_PATH" ] || [ -L "$BACKUP_PATH" ]; then
            print_error "$(msg 'backup_failed'): $BACKUP_PATH"
            fail_install
        fi
        print_info "$(msg 'backup_created'): $BACKUP_PATH"
    fi
    # The destination is the executable itself, not a directory to move into: a
    # directory planted there would otherwise silently receive the staged binary
    # and the install would report success while the executable is a directory.
    # This refuses that portably - `mv -T` would say the same thing, but BSD/macOS
    # mv does not have it and this installer also runs on darwin. Renaming onto a
    # symlink is safe without it: rename(2) replaces the link, it does not follow
    # it.
    if [ -d "$INSTALL_DIR/sub2api" ]; then
        print_error "$(msg 'binary_target_invalid'): $INSTALL_DIR/sub2api"
        fail_install
    fi
    mv -f "$staged" "$INSTALL_DIR/sub2api"
    # Fail closed if that path is a directory after the rename: a directory planted
    # between the check above and the rename would have received the binary instead
    # of being replaced by it. The rename itself cannot be made non-clobbering and
    # directory-safe portably, so the race is detected here rather than prevented:
    # the install is reported as failed, never as a completed swap.
    if [ ! -f "$INSTALL_DIR/sub2api" ] || [ -d "$INSTALL_DIR/sub2api" ]; then
        print_error "$(msg 'binary_target_invalid'): $INSTALL_DIR/sub2api"
        fail_install
    fi
    # The replacement is in place and the parked original was copied to BACKUP_PATH
    # (verified) above, so it is no longer the only copy of the installed binary and
    # no abort path should put it back over the binary that was just installed.
    PARKED_ORIGINAL=""
    # From here on the install path holds the new binary, so any later abort that
    # starts the service is starting that binary, not restoring the previous one.
    SWAP_COMPLETED="true"

    # Record the version of the release that was just installed, with the hash of the
    # executable it was installed as, so that later runs report it without executing
    # the installed executable and stop reporting it once that executable has been
    # replaced (see version_stamp_path). A failure here is reported but does not
    # abort: the installation itself is complete, and only the recorded version is
    # missing.
    if ! write_version_stamp "$LATEST_VERSION" "$INSTALLED_BINARY_SHA256"; then
        print_warning "$(msg 'version_stamp_failed')"
    fi

    rm -rf "$STAGE_DIR"
    STAGE_DIR=""

    # Copy deploy files if they exist in the archive. A member named like the
    # executable is skipped: deploy files are flattened into INSTALL_DIR, so
    # otherwise an archive member could overwrite the verified binary that was
    # just installed.
    #
    # INSTALL_DIR belongs to the service account, so an entry in it can be a link
    # that account planted at a deploy file's name, aimed at any file on the host.
    # A root `cp` onto that name writes *through* the link instead of replacing it
    # (measured: the link target is rewritten and the link stays in place), which
    # would hand that account a root write on a path it cannot touch itself. A
    # planted directory is the sibling hazard: `mv` moves an operand *into* an
    # existing directory instead of onto it, and follows a link to one. Each member
    # is therefore staged in the root-owned staging directory and renamed onto its
    # name - a rename replaces the directory entry and never follows it - while a
    # name that is already a directory is left alone and reported. These files are
    # auxiliary (install-datamanagementd.sh and apple-container.sh read them from
    # beside the installed binary, while the executable, the service unit and the
    # configuration are installed by the steps above), so a member that cannot be
    # placed safely is skipped and named rather than failing a completed install.
    if [ -d "$TEMP_DIR/extract/deploy" ]; then
        local deploy_file deploy_name deploy_staged
        # A private staging directory beside INSTALL_DIR, on the same filesystem so
        # the renames below stay renames. The one the binary was staged in has been
        # removed by now (the swap is done); the EXIT trap removes this one again.
        if ! STAGE_DIR=$(mktemp -d "$(dirname "$INSTALL_DIR")/.sub2api.stage.XXXXXXXX"); then
            STAGE_DIR=""
            print_warning "$(msg 'deploy_staging_failed')"
        fi
        if [ -n "$STAGE_DIR" ]; then
            for deploy_file in "$TEMP_DIR/extract/deploy"/*; do
                [ -e "$deploy_file" ] || continue
                deploy_name=$(basename "$deploy_file")
                if [ "$deploy_name" = "sub2api" ]; then
                    print_warning "$(msg 'deploy_member_skipped'): deploy/sub2api"
                    continue
                fi
                # Only regular files are installed. The archive also carries
                # deploy/tests, and `cp -r` would have copied that tree into the
                # install directory; the tests are not runtime assets, and a link
                # here is not something extraction produces (it refuses link
                # members). This is the normal shape of the archive, so it is
                # skipped quietly rather than reported as a problem.
                if [ ! -f "$deploy_file" ] || [ -L "$deploy_file" ]; then
                    continue
                fi
                # A directory at that name - or a link to one, which `mv` follows -
                # would receive the file inside its target and keep the name a
                # directory. It is named and left alone rather than written into.
                if [ -d "$INSTALL_DIR/$deploy_name" ]; then
                    print_warning "$(msg 'deploy_member_unsafe'): deploy/$deploy_name"
                    continue
                fi
                deploy_staged="$STAGE_DIR/$deploy_name"
                # Stage, then rename. The result is checked after the move as well:
                # a destination that became a directory in between must be reported
                # as skipped, never as a deploy file that is not there.
                if cp "$deploy_file" "$deploy_staged" &&
                    [ -f "$deploy_staged" ] && [ ! -L "$deploy_staged" ] &&
                    mv -f "$deploy_staged" "$INSTALL_DIR/$deploy_name" &&
                    [ -f "$INSTALL_DIR/$deploy_name" ] && [ ! -L "$INSTALL_DIR/$deploy_name" ]; then
                    continue
                fi
                rm -f "$deploy_staged"
                print_warning "$(msg 'deploy_member_unsafe'): deploy/$deploy_name"
            done
        fi
    fi

    print_success "$(msg 'binary_installed') $INSTALL_DIR/sub2api"
}

# Create system user
create_user() {
    if id "$SERVICE_USER" &>/dev/null; then
        print_info "$(msg 'user_exists'): $SERVICE_USER"
        # Fix: Ensure existing user has /bin/sh shell for sudo to work
        # Previous versions used /bin/false which prevents sudo execution
        local current_shell
        current_shell=$(getent passwd "$SERVICE_USER" 2>/dev/null | cut -d: -f7)
        if [ "$current_shell" = "/bin/false" ] || [ "$current_shell" = "/sbin/nologin" ]; then
            print_info "Fixing user shell for sudo compatibility..."
            if usermod -s /bin/sh "$SERVICE_USER" 2>/dev/null; then
                print_success "User shell updated to /bin/sh"
            else
                print_warning "Failed to update user shell. Service restart may not work automatically."
                print_warning "Manual fix: sudo usermod -s /bin/sh $SERVICE_USER"
            fi
        fi
    else
        print_info "$(msg 'creating_user') $SERVICE_USER..."
        # Use /bin/sh instead of /bin/false to allow sudo execution
        # The user still cannot login interactively (no password set)
        useradd -r -s /bin/sh -d "$INSTALL_DIR" "$SERVICE_USER"
        print_success "$(msg 'user_created')"
    fi
}

# Setup directories and permissions
setup_directories() {
    print_info "$(msg 'setting_up_dirs')"

    # Create directories
    mkdir -p "$INSTALL_DIR"
    mkdir -p "$INSTALL_DIR/data"
    mkdir -p "$CONFIG_DIR"

    # Set ownership
    chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"
    chown -R "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR"

    print_success "$(msg 'dirs_configured')"
}

# Install systemd service
install_service() {
    print_info "$(msg 'installing_service')"

    # Create service file with configured host and port
    cat > /etc/systemd/system/sub2api.service << EOF
[Unit]
Description=Sub2API - AI API Gateway Platform
Documentation=https://github.com/${GITHUB_REPO}
After=network.target postgresql.service redis.service
Wants=postgresql.service redis.service

[Service]
Type=simple
User=sub2api
Group=sub2api
WorkingDirectory=/opt/sub2api
ExecStart=/opt/sub2api/sub2api
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=sub2api

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/opt/sub2api

# Environment - Server configuration
Environment=GIN_MODE=release
Environment=SERVER_HOST=${SERVER_HOST}
Environment=SERVER_PORT=${SERVER_PORT}

[Install]
WantedBy=multi-user.target
EOF

    # Reload systemd
    systemctl daemon-reload

    print_success "$(msg 'service_installed')"
}

# Prepare for setup wizard (no config file needed - setup wizard will create it)
prepare_for_setup() {
    print_success "$(msg 'ready_for_setup')"
}

# Get public IP address
#
# Best-effort and advisory: the address is only used in the completion banner, so
# neither a failing lookup nor a response without an address may fail the install.
# The lookup failing is exactly the case the fallback below exists for, and it
# must not abort through `set -e` on the way there - a fresh install would then
# stop after writing the unit file, before the service is ever started.
get_public_ip() {
    print_info "$(msg 'getting_public_ip')"

    # Try to get public IP from ipinfo.io
    local response
    response=$(curl -s --connect-timeout 5 --max-time 10 "https://ipinfo.io/json" 2>/dev/null || true)

    if [ -n "$response" ]; then
        # Extract IP from JSON response using grep and sed (no jq dependency)
        PUBLIC_IP=$(echo "$response" | grep -o '"ip": *"[^"]*"' | sed 's/"ip": *"\([^"]*\)"/\1/')
        if [ -n "$PUBLIC_IP" ]; then
            print_success "Public IP: $PUBLIC_IP"
            return 0
        fi
    fi

    # Fallback to local IP
    print_warning "$(msg 'public_ip_failed')"
    PUBLIC_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "YOUR_SERVER_IP")
    return 0
}

# Start the service, or restart it when it is already running
#
# After the binary has been swapped, a plain "start" on an already-running
# service is a no-op and the OLD process would keep running while the installer
# reported success. Restarting makes the freshly installed binary the one in use;
# a failure is reported and returned so callers never claim a success they did
# not observe.
start_service() {
    print_info "$(msg 'starting_service')"

    if systemctl is-active --quiet sub2api; then
        if systemctl restart sub2api; then
            print_success "$(msg 'service_restarted')"
            return 0
        fi
        print_error "$(msg 'service_start_failed')"
        print_info "sudo journalctl -u sub2api -n 50"
        return 1
    fi

    if systemctl start sub2api; then
        print_success "$(msg 'service_started')"
        return 0
    fi
    print_error "$(msg 'service_start_failed')"
    print_info "sudo journalctl -u sub2api -n 50"
    return 1
}

# Enable service auto-start
enable_autostart() {
    print_info "$(msg 'enabling_autostart')"

    if systemctl enable sub2api 2>/dev/null; then
        print_success "$(msg 'autostart_enabled')"
        return 0
    else
        print_warning "Failed to enable auto-start"
        return 1
    fi
}

# Finish a fresh install: bring the service up and only then claim success, so a
# failed start is never reported as a completed installation.
finish_fresh_install() {
    if ! start_service; then
        print_error "$(msg 'service_not_restarted')"
        exit 1
    fi
    enable_autostart
    print_completion
}

# Print completion message
print_completion() {
    # Use PUBLIC_IP which was set by get_public_ip()
    # Determine display address
    local display_host="${PUBLIC_IP:-YOUR_SERVER_IP}"
    if [ "$SERVER_HOST" = "127.0.0.1" ]; then
        display_host="127.0.0.1"
    fi

    echo ""
    echo "=============================================="
    print_success "$(msg 'install_complete')"
    echo "=============================================="
    echo ""
    echo "$(msg 'install_dir'): $INSTALL_DIR"
    echo "$(msg 'server_config_summary'): ${SERVER_HOST}:${SERVER_PORT}"
    echo ""
    echo "=============================================="
    echo "  $(msg 'step4_open_wizard')"
    echo "=============================================="
    echo ""
    print_info "     http://${display_host}:${SERVER_PORT}"
    echo ""
    echo "     $(msg 'wizard_guide')"
    echo "     - $(msg 'wizard_db')"
    echo "     - $(msg 'wizard_redis')"
    echo "     - $(msg 'wizard_admin')"
    echo ""
    echo "=============================================="
    echo "  $(msg 'useful_commands')"
    echo "=============================================="
    echo ""
    echo "  $(msg 'cmd_status'):   sudo systemctl status sub2api"
    echo "  $(msg 'cmd_logs'):     sudo journalctl -u sub2api -f"
    echo "  $(msg 'cmd_restart'):  sudo systemctl restart sub2api"
    echo "  $(msg 'cmd_stop'):     sudo systemctl stop sub2api"
    echo ""
    echo "=============================================="
}

# Upgrade function
upgrade() {
    # Check if Sub2API is installed
    if [ ! -f "$INSTALL_DIR/sub2api" ]; then
        print_error "$(msg 'not_installed')"
        print_info "$(msg 'fresh_install_hint'): $0 install"
        exit 1
    fi

    print_info "$(msg 'upgrading')"

    # Resolve and validate the target release BEFORE stopping the service: an
    # image-only release, a missing platform asset or a network failure must fail
    # closed without taking the running service down.
    get_latest_version

    # Get current version. Read from the version stamp, never by executing the
    # installed executable: that file can be replaced by the service account, so
    # running it here would run that account's code as root.
    CURRENT_VERSION=$(get_current_version)
    print_info "$(msg 'current_version'): $CURRENT_VERSION"

    # Stop service (only the swap below needs the process down)
    if systemctl is-active --quiet sub2api; then
        print_info "$(msg 'stopping_service')"
        systemctl stop sub2api
        SERVICE_STOPPED_FOR_INSTALL="true"
    fi

    # The previous binary is backed up by download_and_extract, after the release
    # has passed verification and immediately before the swap. Backing it up here
    # would already have overwritten the operator's previous rollback copy even
    # when this release never installs.
    BACKUP_PATH="$INSTALL_DIR/sub2api.backup"

    # Download, verify and swap. A failure here leaves the previous binary in
    # place (the swap is the last step) and restores the service.
    download_and_extract

    # Start (or restart) so the new binary is the one running. The stop marker is
    # cleared only once the service is confirmed up, so any abort before that
    # point is repaired by the EXIT trap.
    if ! start_service; then
        print_error "$(msg 'service_not_restarted')"
        exit 1
    fi
    SERVICE_STOPPED_FOR_INSTALL=""

    print_success "$(msg 'upgrade_complete')"
}

# Install specific version (for upgrade or rollback)
# Requires: Sub2API must already be installed
install_version() {
    local target_version="$1"

    # Check if Sub2API is installed
    if [ ! -f "$INSTALL_DIR/sub2api" ]; then
        print_error "$(msg 'not_installed')"
        print_info "$(msg 'fresh_install_hint'): $0 install -v $target_version"
        exit 1
    fi

    # Validate and normalize version
    target_version=$(validate_version "$target_version")

    print_info "$(msg 'installing_version'): $target_version"

    # Get current version
    local current_version
    current_version=$(get_current_version)
    print_info "$(msg 'current_version'): $current_version"

    # Check if same version. This skips the install, so it may only fire when the
    # version really is the one installed: get_current_version reports a version
    # only while the recorded stamp still matches the executable at the install
    # path, and reports "unknown" - which matches no target here - once that
    # executable has been replaced out of band (by the in-app updater, say) and the
    # stamp has gone stale. A stale stamp therefore cannot turn a requested rollback
    # into a silent no-op that reports success for a version it never installed.
    if [ "$current_version" = "$target_version" ] || [ "$current_version" = "${target_version#v}" ]; then
        print_warning "$(msg 'same_version')"
        exit 0
    fi

    # Stop service if running (validation above already passed, and a failure
    # below restores it)
    if systemctl is-active --quiet sub2api; then
        print_info "$(msg 'stopping_service')"
        systemctl stop sub2api
        SERVICE_STOPPED_FOR_INSTALL="true"
    fi

    # The previous binary is backed up by download_and_extract, after the release
    # has passed verification and immediately before the swap, so a rollback that
    # never installs leaves the backup path untouched.
    #
    # The name records the version of the binary that is being replaced only when
    # that version is known: get_current_version reports one only while the stamp
    # still matches the installed executable, so a version that reaches this branch
    # is the version of the file this backup will contain. Once the executable was
    # replaced out of band the recorded version no longer describes it, and naming
    # the backup after that stale version would label the wrong bytes as a rollback
    # point for a version they are not - so an unverified version names nothing and
    # the timestamp keeps the two backups of the same run apart instead.
    if [ "$current_version" != "unknown" ] && [ "$current_version" != "not_installed" ]; then
        BACKUP_PATH="$INSTALL_DIR/sub2api.backup.${current_version}"
    else
        BACKUP_PATH="$INSTALL_DIR/sub2api.backup.$(date +%Y%m%d%H%M%S)"
    fi

    # Set LATEST_VERSION to the target version for download_and_extract
    LATEST_VERSION="$target_version"

    # Download and install
    download_and_extract

    # Start (or restart) so the installed version is the one running. The stop
    # marker is cleared only once the service is confirmed up, so any abort before
    # that point is repaired by the EXIT trap. The completion banner below is only
    # printed when the service really came up.
    if ! start_service; then
        print_error "$(msg 'service_not_restarted')"
        exit 1
    fi
    SERVICE_STOPPED_FOR_INSTALL=""

    # Print completion message
    local new_version
    new_version=$(get_current_version)
    echo ""
    echo "=============================================="
    print_success "$(msg 'install_version_complete')"
    echo "=============================================="
    echo ""
    echo "  $(msg 'current_version'): $new_version"
    echo ""
}

# Uninstall function
uninstall() {
    print_warning "$(msg 'uninstall_confirm')"

    # If not interactive (piped), require -y flag or skip confirmation
    if ! is_interactive; then
        if [ "${FORCE_YES:-}" != "true" ]; then
            print_error "Non-interactive mode detected. Use 'curl ... | bash -s -- uninstall -y' to confirm."
            exit 1
        fi
    else
        read -p "$(msg 'are_you_sure') " -n 1 -r < /dev/tty
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            print_info "$(msg 'uninstall_cancelled')"
            exit 0
        fi
    fi

    print_info "$(msg 'stopping_service')"
    systemctl stop sub2api 2>/dev/null || true
    systemctl disable sub2api 2>/dev/null || true

    print_info "$(msg 'removing_files')"
    rm -f /etc/systemd/system/sub2api.service
    systemctl daemon-reload

    print_info "$(msg 'removing_install_dir')"
    rm -rf "$INSTALL_DIR"
    # The stamp records the version of the executable that was just removed, so it
    # goes with it: left behind, it would describe an installation that is gone.
    rm -f "$(version_stamp_path)" 2>/dev/null || true

    print_info "$(msg 'removing_user')"
    userdel "$SERVICE_USER" 2>/dev/null || true

    # Remove install lock file (.installed) to allow fresh setup on reinstall
    print_info "$(msg 'removing_install_lock')"
    rm -f "$CONFIG_DIR/.installed" 2>/dev/null || true
    rm -f "$INSTALL_DIR/.installed" 2>/dev/null || true
    print_success "$(msg 'install_lock_removed')"

    # Ask about config directory removal (interactive mode only)
    local remove_config=false
    if [ "${PURGE:-}" = "true" ]; then
        remove_config=true
    elif is_interactive; then
        read -p "$(msg 'purge_prompt')" -n 1 -r < /dev/tty
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            remove_config=true
        fi
    fi

    if [ "$remove_config" = true ]; then
        print_info "$(msg 'removing_config_dir')"
        rm -rf "$CONFIG_DIR"
    else
        print_warning "$(msg 'config_not_removed'): $CONFIG_DIR"
        print_warning "$(msg 'remove_manually')"
    fi

    print_success "$(msg 'uninstall_complete')"
}

# Main
main() {
    # Parse flags first
    local target_version=""
    local positional_args=()

    while [[ $# -gt 0 ]]; do
        case "$1" in
            -y|--yes)
                FORCE_YES="true"
                shift
                ;;
            --purge)
                PURGE="true"
                shift
                ;;
            -v|--version)
                if [ -n "${2:-}" ] && [[ ! "$2" =~ ^- ]]; then
                    target_version="$2"
                    shift 2
                else
                    echo "Error: --version requires a version argument"
                    exit 1
                fi
                ;;
            --version=*)
                target_version="${1#*=}"
                if [ -z "$target_version" ]; then
                    echo "Error: --version requires a version argument"
                    exit 1
                fi
                shift
                ;;
            *)
                positional_args+=("$1")
                shift
                ;;
        esac
    done

    # Restore positional arguments
    set -- "${positional_args[@]}"

    # Select language first
    select_language

    echo ""
    echo "=============================================="
    echo "       $(msg 'install_title')"
    echo "=============================================="
    echo ""

    # Parse commands
    case "${1:-}" in
        upgrade|update)
            check_root
            detect_platform
            check_dependencies
            if [ -n "$target_version" ]; then
                # Upgrade to specific version
                install_version "$target_version"
            else
                # Upgrade to latest
                upgrade
            fi
            exit 0
            ;;
        install)
            # Install with optional version
            check_root
            detect_platform
            check_dependencies
            if [ -n "$target_version" ]; then
                # Install specific version (fresh install or rollback)
                if [ -f "$INSTALL_DIR/sub2api" ]; then
                    # Already installed, treat as version change
                    install_version "$target_version"
                else
                    # Fresh install with specific version
                    configure_server
                    LATEST_VERSION=$(validate_version "$target_version")
                    download_and_extract
                    create_user
                    setup_directories
                    install_service
                    prepare_for_setup
                    get_public_ip
                    finish_fresh_install
                fi
            else
                # Latest version. This runs over an existing installation as well,
                # and the swap below replaces the installed binary by rename, so
                # the binary being replaced is kept when there is one.
                configure_server
                get_latest_version
                backup_path_for_existing_install
                download_and_extract
                create_user
                setup_directories
                install_service
                prepare_for_setup
                get_public_ip
                finish_fresh_install
            fi
            exit 0
            ;;
        rollback)
            # Rollback to a specific version (alias for install with version)
            if [ -z "$target_version" ] && [ -n "${2:-}" ]; then
                target_version="$2"
            fi
            # The candidate list is platform-filtered, so the platform has to be
            # detected before listing even when no version was given.
            check_root
            detect_platform
            check_dependencies
            if [ -z "$target_version" ]; then
                print_error "$(msg 'opt_version')"
                echo ""
                echo "Usage: $0 rollback -v <version>"
                echo "       $0 rollback <version>"
                echo ""
                list_versions
                exit 1
            fi
            install_version "$target_version"
            exit 0
            ;;
        list-versions|versions)
            # The candidate list is platform-dependent (it only shows releases
            # with an installable archive for this machine), so the platform has
            # to be detected before listing.
            detect_platform
            check_dependencies
            list_versions
            exit 0
            ;;
        uninstall|remove)
            check_root
            uninstall
            exit 0
            ;;
        --help|-h)
            echo "$(msg 'usage'): $0 [command] [options]"
            echo ""
            echo "Commands:"
            echo "  $(msg 'cmd_none')            $(msg 'cmd_install')"
            echo "  install              $(msg 'cmd_install')"
            echo "  upgrade              $(msg 'cmd_upgrade')"
            echo "  rollback <version>   $(msg 'cmd_install_version')"
            echo "  list-versions        $(msg 'cmd_list_versions')"
            echo "  uninstall            $(msg 'cmd_uninstall')"
            echo ""
            echo "Options:"
            echo "  -v, --version <ver>  $(msg 'opt_version')"
            echo "  -y, --yes            Skip confirmation prompts (for uninstall)"
            echo ""
            echo "Release source: https://github.com/${GITHUB_REPO}"
            echo "$(msg 'binary_only_scope')"
            echo "$(msg 'docker_image_managed')"
            echo "Install and rollback accept only fork releases that publish the"
            echo "${OS:-current-platform} archive plus ${CHECKSUMS_ASSET_NAME}; missing or"
            echo "mismatching checksums abort before ${INSTALL_DIR} is modified."
            echo ""
            echo "Examples:"
            echo "  $0                        # Install latest installable fork release"
            echo "  $0 install -v v0.1.0      # Install specific version"
            echo "  $0 upgrade                # Upgrade to latest"
            echo "  $0 upgrade -v v0.2.0      # Upgrade to specific version"
            echo "  $0 rollback v0.1.0        # Rollback to v0.1.0 (must be a listed candidate)"
            echo "  $0 list-versions          # List installable versions (this platform only)"
            echo ""
            exit 0
            ;;
    esac

    # Default: Fresh install with latest version
    check_root
    detect_platform
    check_dependencies

    if [ -n "$target_version" ]; then
        # Install specific version
        if [ -f "$INSTALL_DIR/sub2api" ]; then
            install_version "$target_version"
        else
            configure_server
            LATEST_VERSION=$(validate_version "$target_version")
            download_and_extract
            create_user
            setup_directories
            install_service
            prepare_for_setup
            get_public_ip
            finish_fresh_install
        fi
    else
        # Install the latest version. As above, this can run over an existing
        # installation, so the binary the swap replaces is kept when there is one.
        configure_server
        get_latest_version
        backup_path_for_existing_install
        download_and_extract
        create_user
        setup_directories
        install_service
        prepare_for_setup
        get_public_ip
        finish_fresh_install
    fi
}

main "$@"
