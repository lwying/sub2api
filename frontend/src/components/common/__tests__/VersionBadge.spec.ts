import { existsSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import VersionBadge from '../VersionBadge.vue'
import { performUpdate } from '@/api/admin/system'

const h = vi.hoisted(() => ({
  state: {
    isAdmin: true,
    hasUpdate: false,
    buildType: 'release',
    versionWarning: '',
    versionCheckFailed: false,
    binaryUpdateSupported: true,
    deploymentType: 'native'
  },
  fetchVersion: vi.fn(),
  clearVersionCache: vi.fn(),
  getRollbackVersions: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    // 已知 key 直接回显，带参数时附带参数，便于断言命令内容与部署方式文案。
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}(${JSON.stringify(params)})` : key,
      te: () => true
    })
  }
})

vi.mock('@/stores', () => ({
  useAuthStore: () => ({ isAdmin: h.state.isAdmin }),
  useAppStore: () => ({
    versionLoading: false,
    currentVersion: '0.2.6',
    latestVersion: '0.2.6',
    hasUpdate: h.state.hasUpdate,
    releaseInfo: undefined,
    buildType: h.state.buildType,
    versionWarning: h.state.versionWarning,
    versionCheckFailed: h.state.versionCheckFailed,
    binaryUpdateSupported: h.state.binaryUpdateSupported,
    deploymentType: h.state.deploymentType,
    fetchVersion: h.fetchVersion,
    clearVersionCache: h.clearVersionCache
  })
}))

vi.mock('@/api/admin/system', () => ({
  performUpdate: vi.fn(),
  restartService: vi.fn(),
  rollback: vi.fn(),
  getRollbackVersions: h.getRollbackVersions
}))

vi.mock('@/composables/useClipboard', async () => {
  const { ref } = await import('vue')
  return {
    useClipboard: () => ({ copied: ref(false), copyToClipboard: vi.fn() })
  }
})

const ROLLBACK_VERSION = '0.2.6'
const FORK_REPO = 'lwying/sub2api'

function findButton(wrapper: VueWrapper, label: string) {
  const button = wrapper.findAll('button').find((item) => item.text().trim() === label)
  if (!button) throw new Error(`button not found: ${label}`)
  return button
}

// The candidate button carries the version plus its publish date, so it is the
// button whose text starts with the tag but is not the badge button itself.
function findVersionCandidate(wrapper: VueWrapper, version: string) {
  const button = wrapper.findAll('button').find((item) => {
    const text = item.text().trim()
    return text.startsWith(`v${version}`) && text !== `v${version}`
  })
  if (!button) throw new Error(`version candidate v${version} not found`)
  return button
}

async function openRollbackPanel(versions: Array<Record<string, string>> = []) {
  h.getRollbackVersions.mockResolvedValue({
    versions: versions.map((item) => ({
      version: item.version,
      published_at: item.published_at ?? '2026-09-01T00:00:00Z',
      html_url: item.html_url ?? `https://github.com/${FORK_REPO}/releases/tag/v${item.version}`
    }))
  })
  const wrapper = mount(VersionBadge, { props: { version: '0.2.6' } })
  await findButton(wrapper, 'v0.2.6').trigger('click')
  await findButton(wrapper, 'version.rollback').trigger('click')
  await flushPromises()
  return wrapper
}

// Opens the rollback panel, selects the candidate version and switches to the
// requested manual-command tab, then returns the rendered command.
async function manualCommandFor(wrapper: VueWrapper, tab: 'script' | 'docker') {
  await findVersionCandidate(wrapper, ROLLBACK_VERSION).trigger('click')
  if (tab === 'docker') {
    await findButton(wrapper, 'version.deployDocker').trigger('click')
  }
  await flushPromises()
  return wrapper.find('code').text()
}

describe('VersionBadge rollback guidance', () => {
  it('points the script rollback command at the fork release tag', async () => {
    const wrapper = await openRollbackPanel([{ version: ROLLBACK_VERSION }])
    const command = await manualCommandFor(wrapper, 'script')

    expect(command).toBe(
      `curl -sSL https://raw.githubusercontent.com/${FORK_REPO}/v${ROLLBACK_VERSION}/deploy/install.sh | sudo bash -s -- rollback v${ROLLBACK_VERSION}`
    )
    expect(command).not.toContain('Wei-Shaw')
  })

  it('keeps the rollback entry reachable while an update is pending', async () => {
    h.state.hasUpdate = true
    h.getRollbackVersions.mockResolvedValue({ versions: [] })
    try {
      const wrapper = mount(VersionBadge, { props: { version: '0.2.6' } })
      await findButton(wrapper, 'v0.2.6').trigger('click')

      // 「有新版本」时仍须能进入回退入口，否则用户无法在升级前回退
      await findButton(wrapper, 'version.rollback').trigger('click')
      await flushPromises()

      expect(wrapper.text()).toContain('version.noRollbackVersions')
    } finally {
      h.state.hasUpdate = false
    }
  })

  it('never advertises a docker image this build cannot verify', async () => {
    const wrapper = await openRollbackPanel([{ version: ROLLBACK_VERSION }])
    const command = await manualCommandFor(wrapper, 'docker')

    // 运维方自行发布的镜像标签占位符，而不是本界面编造的镜像
    expect(command).toContain('version.dockerImagePlaceholder')
    // GoReleaser 的 {{ .Version }} 去掉 v 前缀，镜像标签就是 0.2.6，不是 v0.2.6
    expect(command).toContain(`version.dockerImagePlaceholder:${ROLLBACK_VERSION}`)
    expect(command).not.toContain(`:v${ROLLBACK_VERSION}`)
    expect(command).toContain('docker compose up -d')
    expect(command).toContain('version.dockerOperatorManaged')
    // 不得出现上游镜像、被编造的 fork 镜像或任何具体 registry 地址
    expect(command).not.toMatch(/weishaw|Wei-Shaw/i)
    expect(command).not.toMatch(/ghcr\.io|docker\.io|registry/)
    expect(command).not.toMatch(new RegExp(`${FORK_REPO}:`))
    // docker 路径不应给出脚本安装命令
    expect(command).not.toContain('install.sh')
  })

  it('warns per deployment method instead of calling in-container replacement an upgrade', async () => {
    const wrapper = await openRollbackPanel([{ version: ROLLBACK_VERSION }])
    await findVersionCandidate(wrapper, ROLLBACK_VERSION).trigger('click')
    await flushPromises()

    // 脚本部署：说明会替换程序并需要重启
    expect(wrapper.text()).toContain('version.rollbackWarning')
    expect(wrapper.text()).not.toContain('version.rollbackWarningDocker')

    await findButton(wrapper, 'version.deployDocker').trigger('click')
    await flushPromises()

    // Docker：说明按镜像标签升级，容器内替换不算升级
    expect(wrapper.text()).toContain('version.rollbackWarningDocker')
  })

  it('keeps the source-build hint and fetches no rollback candidates', async () => {
    h.state.buildType = 'source'
    h.state.binaryUpdateSupported = false
    h.getRollbackVersions.mockClear()
    try {
      const wrapper = mount(VersionBadge, { props: { version: '0.2.6' } })
      await findButton(wrapper, 'v0.2.6').trigger('click')
      await findButton(wrapper, 'version.rollback').trigger('click')
      await flushPromises()

      expect(wrapper.text()).toContain('version.rollbackSourceHint')
      expect(h.getRollbackVersions).not.toHaveBeenCalled()
    } finally {
      h.state.buildType = 'release'
      h.state.binaryUpdateSupported = true
    }
  })

  it('renders the plain version for non-admins', () => {
    h.state.isAdmin = false
    try {
      const wrapper = mount(VersionBadge, { props: { version: '0.2.6' } })
      expect(wrapper.text()).toBe('v0.2.6')
      expect(wrapper.findAll('button')).toHaveLength(0)
    } finally {
      h.state.isAdmin = true
    }
  })
})

describe('VersionBadge unknown update state', () => {
  async function openBadge() {
    const wrapper = mount(VersionBadge, { props: { version: '0.2.6' } })
    await findButton(wrapper, 'v0.2.6').trigger('click')
    await flushPromises()
    return wrapper
  }

  it('does not claim the latest version when the check reported a warning', async () => {
    h.state.versionWarning = 'this release ships no binary for this platform'
    try {
      const wrapper = await openBadge()

      // 后端明确给出 warning（例如只有镜像没有二进制资产）时不得声称已是最新
      expect(wrapper.text()).not.toContain('version.upToDate')
      expect(wrapper.text()).toContain('version.checkUnavailable')
    } finally {
      h.state.versionWarning = ''
    }
  })

  it('does not claim the latest version when the check itself failed', async () => {
    h.state.versionCheckFailed = true
    try {
      const wrapper = await openBadge()

      expect(wrapper.text()).not.toContain('version.upToDate')
      expect(wrapper.text()).toContain('version.checkUnavailable')
    } finally {
      h.state.versionCheckFailed = false
    }
  })

  it('still claims the latest version on a normal cached result', async () => {
    const wrapper = await openBadge()

    expect(wrapper.text()).toContain('version.upToDate')
    expect(wrapper.text()).not.toContain('version.checkUnavailable')
  })
})

describe('VersionBadge in-app binary update capability', () => {
  async function openBadge() {
    const wrapper = mount(VersionBadge, { props: { version: '0.2.6' } })
    await findButton(wrapper, 'v0.2.6').trigger('click')
    await flushPromises()
    return wrapper
  }

  it('offers the in-app update only when the backend explicitly supports it', async () => {
    h.state.hasUpdate = true
    h.state.binaryUpdateSupported = true
    try {
      const wrapper = await openBadge()
      expect(wrapper.text()).toContain('version.updateNow')
    } finally {
      h.state.hasUpdate = false
    }
  })

  it('does not offer a binary swap on a Docker deployment even when an update exists', async () => {
    h.state.hasUpdate = true
    h.state.binaryUpdateSupported = false
    h.state.deploymentType = 'docker'
    try {
      const wrapper = await openBadge()

      // build_type 仍是 release，但 Docker 不能靠替换容器内程序升级
      expect(h.state.buildType).toBe('release')
      expect(wrapper.text()).not.toContain('version.updateNow')
      expect(wrapper.text()).toContain('version.updateDockerHint')
      expect(wrapper.text()).not.toContain('version.sourceModeHint')
    } finally {
      h.state.hasUpdate = false
      h.state.deploymentType = 'native'
    }
  })

  it('does not offer a binary swap when the response omits the capability flag', async () => {
    h.state.hasUpdate = true
    h.state.binaryUpdateSupported = false
    try {
      const wrapper = await openBadge()

      // 旧后端没有该字段：保守处理，不提供应用内替换
      expect(wrapper.text()).not.toContain('version.updateNow')
      expect(wrapper.text()).toContain('version.updateUnsupportedHint')
    } finally {
      h.state.hasUpdate = false
    }
  })

  it('does not fetch in-app rollback candidates for a Docker deployment', async () => {
    h.state.binaryUpdateSupported = false
    h.state.deploymentType = 'docker'
    h.getRollbackVersions.mockClear()
    try {
      const wrapper = await openBadge()
      await findButton(wrapper, 'version.rollback').trigger('click')
      await flushPromises()

      expect(wrapper.text()).toContain('version.rollbackDockerHint')
      expect(wrapper.text()).not.toContain('version.rollbackSourceHint')
      expect(h.getRollbackVersions).not.toHaveBeenCalled()
    } finally {
      h.state.deploymentType = 'native'
      h.state.binaryUpdateSupported = true
    }
  })

  it('reports nothing installed when the backend answers already up to date', async () => {
    h.state.hasUpdate = true
    h.state.binaryUpdateSupported = true
    vi.mocked(performUpdate).mockResolvedValue({
      message: 'Already up to date',
      need_restart: false,
      already_up_to_date: true
    })
    try {
      const wrapper = await openBadge()
      await findButton(wrapper, 'version.updateNow').trigger('click')
      await flushPromises()

      // HTTP 200 但没有安装任何东西：不得显示「更新完成」或要求重启
      expect(wrapper.text()).not.toContain('version.updateComplete')
      expect(wrapper.text()).not.toContain('version.restartRequired')
      expect(wrapper.text()).toContain('version.updateNothingToInstall')
      // 未安装就不该清空版本缓存，而应重新核对真实状态
      expect(h.fetchVersion).toHaveBeenCalled()
      expect(h.clearVersionCache).not.toHaveBeenCalled()
    } finally {
      h.state.hasUpdate = false
      vi.mocked(performUpdate).mockReset()
      h.fetchVersion.mockClear()
      h.clearVersionCache.mockClear()
    }
  })
})

describe('VersionBadge release source', () => {
  it('resolves releases from the fork only', () => {
    // vitest 在 frontend/ 下运行；兼容从仓库根目录启动的情况。
    const candidates = [
      resolve(process.cwd(), 'src/components/common/VersionBadge.vue'),
      resolve(process.cwd(), 'frontend/src/components/common/VersionBadge.vue')
    ]
    const sourcePath = candidates.find((candidate) => existsSync(candidate))
    if (!sourcePath) throw new Error(`VersionBadge.vue not found in: ${candidates.join(', ')}`)
    const source = readFileSync(sourcePath, 'utf8')

    expect(source).toContain(`const GITHUB_REPO = '${FORK_REPO}'`)
    expect(source).not.toContain('Wei-Shaw')
    expect(source).not.toContain('weishaw')
    // 旧实现里的固定镜像常量不得回归
    expect(source).not.toContain('DOCKER_IMAGE =')
  })
})
