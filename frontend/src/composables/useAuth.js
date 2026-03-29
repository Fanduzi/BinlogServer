// input: auth token storage, auth-required browser events, i18n
// output: settings dialog state, auth banner state, open/save handlers
// pos: auth token management and settings dialog logic
import { computed, ref, onMounted, onBeforeUnmount } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage } from "element-plus";
import { getAuthToken, setAuthToken } from "../utils/auth.js";

export function useAuth(onAfterSave) {
  const { t } = useI18n();

  const settingsVisible = ref(false);
  const settingsToken = ref("");
  const authRequiredMessage = ref("");
  const authRequiredTitle = computed(() => t("auth.reauthRequired"));

  function handleAuthRequired() {
    authRequiredMessage.value = t("auth.tokenExpired");
    settingsVisible.value = true;
    settingsToken.value = getAuthToken() || "";
  }

  function openSettings() {
    settingsToken.value = getAuthToken() || "";
    settingsVisible.value = true;
  }

  function saveSettings() {
    setAuthToken(settingsToken.value);
    settingsVisible.value = false;
    authRequiredMessage.value = "";
    window.__authDialogShown = false;
    ElMessage.success(t("msg.settingsSaved"));
    onAfterSave?.();
  }

  onMounted(() => window.addEventListener("auth-required", handleAuthRequired));
  onBeforeUnmount(() => window.removeEventListener("auth-required", handleAuthRequired));

  return {
    settingsVisible,
    settingsToken,
    authRequiredMessage,
    authRequiredTitle,
    openSettings,
    saveSettings,
  };
}
