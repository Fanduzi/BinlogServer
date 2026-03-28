import { createApp } from "vue";
import {
  ElAlert,
  ElButton,
  ElCard,
  ElCol,
  ElConfigProvider,
  ElDialog,
  ElDrawer,
  ElEmpty,
  ElForm,
  ElFormItem,
  ElInput,
  ElInputNumber,
  ElOption,
  ElPagination,
  ElRow,
  ElSelect,
  ElSwitch,
  ElTable,
  ElTableColumn,
  ElTag,
  ElTimeline,
  ElTimelineItem,
  ElTooltip,
} from "element-plus";
import zhCnLocale from "element-plus/dist/locale/zh-cn.mjs";
import enLocale from "element-plus/dist/locale/en.mjs";
import "element-plus/dist/index.css";
import "@fortawesome/fontawesome-free/css/all.min.css";
import App from "./App.vue";
import { i18n, getLocale } from "./locales";

const app = createApp(App);

// Register i18n
app.use(i18n);

// Register Element Plus components
[
  ElAlert,
  ElButton,
  ElCard,
  ElCol,
  ElConfigProvider,
  ElDialog,
  ElDrawer,
  ElEmpty,
  ElForm,
  ElFormItem,
  ElInput,
  ElInputNumber,
  ElOption,
  ElPagination,
  ElRow,
  ElSelect,
  ElSwitch,
  ElTable,
  ElTableColumn,
  ElTag,
  ElTimeline,
  ElTimelineItem,
  ElTooltip,
].forEach((component) => app.use(component));

app.mount("#app");
