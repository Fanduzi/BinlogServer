import { createApp } from "vue";
import {
  ElAlert,
  ElButton,
  ElCard,
  ElCol,
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
import "element-plus/dist/index.css";
import "@fortawesome/fontawesome-free/css/all.min.css";
import App from "./App.vue";

const app = createApp(App);

[
  ElAlert,
  ElButton,
  ElCard,
  ElCol,
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
