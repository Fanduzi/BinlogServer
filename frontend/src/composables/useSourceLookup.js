// input: source host/port query input from user
// output: reactive sourceQuery, lookup result state
// pos: state container for source coverage filter panel
import { reactive } from "vue";

export function useSourceLookup() {
  const sourceQuery = reactive({
    host: "",
    port: null,
  });

  const lookup = reactive({
    checked: false,
    exists: false,
    count: 0,
  });

  function clearLookupState() {
    sourceQuery.host = "";
    sourceQuery.port = null;
    lookup.checked = false;
    lookup.exists = false;
    lookup.count = 0;
  }

  return { sourceQuery, lookup, clearLookupState };
}
