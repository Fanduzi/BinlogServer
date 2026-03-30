// input: browser window resize events
// output: reactive windowWidth, isMobile flag, menuCollapsed toggle
// pos: global layout state shared across shell and dialogs
import { computed, ref, onMounted, onBeforeUnmount } from "vue";

export function useWindowState() {
  const menuCollapsed = ref(false);
  const windowWidth = ref(typeof window !== "undefined" ? window.innerWidth : 1280);

  function onResize() {
    windowWidth.value = window.innerWidth;
  }

  onMounted(() => window.addEventListener("resize", onResize));
  onBeforeUnmount(() => window.removeEventListener("resize", onResize));

  const isMobile = computed(() => windowWidth.value < 768);

  return { menuCollapsed, windowWidth, isMobile };
}
