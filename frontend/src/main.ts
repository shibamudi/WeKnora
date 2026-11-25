import { createApp } from "vue";
import { createPinia } from "pinia";
import App from "./App.vue";
import router from "./router";
import TDesign from "tdesign-vue-next";
// 引入组件库的少量全局样式变量
import "tdesign-vue-next/dist/tdesign.css";
import "@/assets/theme/theme.css";
import "@/assets/theme/tdesign-overrides.less";
import "@/assets/dropdown-menu.less";
import "@/components/css/chat-hljs-theme.less";
import "@/components/css/assistant-page-fill.less";
// vue-virtual-scroller ships its own tiny stylesheet — required for
// RecycleScroller/DynamicScroller to size their viewport correctly.
// Without it the scroller computes 0 height and renders no items.
import "vue-virtual-scroller/dist/vue-virtual-scroller.css";
import i18n, { initI18n } from "./i18n";
import { initTheme } from "@/composables/useTheme";
import { initFont } from "@/composables/useFont";
import { installTDesignIconOfflineGuard } from "@/utils/tdesign-icon-offline";
import { installAutofillGuard } from "@/utils/disable-autofill";
import { useAuthStore } from "@/stores/auth";
import { trustedSessionLogin } from "@/api/auth";

// 必须在 Vue 组件挂载之前执行，避免 tdesign-icons 运行时请求 tdesign.gtimg.com
installTDesignIconOfflineGuard();

initTheme();
initFont();

async function bootstrap() {
  const app = createApp(App);

  // 全局错误处理：捕获未处理的组件错误，防止白屏
  app.config.errorHandler = (err, instance, info) => {
    console.error("[WeKnora] Unhandled Vue error:", err, "\nComponent:", instance, "\nInfo:", info);
  };

  app.use(TDesign);
  const pinia = createPinia();
  app.use(pinia);

  try {
    await initI18n();
  } catch (err) {
    console.error("[WeKnora] Failed to load initial locale bundle:", err);
  }

  // Capabilities (can_create_tenant, auto_accept_invitation) are not cached
  // in localStorage — reconcile once before first paint when a session exists.
  const authStore = useAuthStore();
  if (localStorage.getItem("weknora_token")) {
    try {
      await authStore.refreshFromAuthMe();
    } catch {
      // best-effort; capabilities stay at defaults until the next refresh
    }
  } else {
    // 可信头部认证(CAS):无本地 token 时,通过 /auth/trusted-session 获取 JWT
    try {
      const response = await trustedSessionLogin();
      if (response.success && response.token) {
        authStore.setToken(response.token);
        if (response.refresh_token) {
          authStore.setRefreshToken(response.refresh_token);
        }
        if (response.user) {
          authStore.setUser(response.user as any);
        }
        if (response.active_tenant) {
          authStore.setTenant({
            id: String(response.active_tenant.id),
            name: response.active_tenant.name || '',
            owner_id: '',
            description: response.active_tenant.description,
            status: response.active_tenant.status || 'active',
            business: response.active_tenant.business || '',
            storage_quota: response.active_tenant.storage_quota || 0,
            storage_used: response.active_tenant.storage_used || 0,
            created_at: response.active_tenant.created_at || new Date().toISOString(),
            updated_at: response.active_tenant.updated_at || new Date().toISOString(),
          });
        }
        if (response.memberships) {
          authStore.setMemberships(response.memberships as any);
        }
      }
    } catch {
      // 可信头部认证未启用或 CAS 会话不存在,正常进入路由守卫流程
    }
  }

  app.use(router);
  app.use(i18n);

  // 等首屏路由（含导航守卫、Lite 自动登录）完成后再挂载，避免先闪默认页再跳转
  await router.isReady();
  app.mount("#app");
  installAutofillGuard();
}

bootstrap();
