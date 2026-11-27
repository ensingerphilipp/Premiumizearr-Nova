<script>
  import { CalculateAPIPath } from "../Utilities/web_root";
  import { DataTable, InlineLoading } from "carbon-components-svelte";


  export let headers = [];
  export let APIpath = "";
  export let updateTimeSeconds = 5;
  export const zebra = true;
  export let totalName = "";
  export let transform = (data) => data; // Default transform function

  let updating = false;
  let status = "";
  let fetchFailed = false;
  $: rows=[]
  $: console.log("Rows updated:", rows);
  $: statusIndicator = updating ? "active" : fetchFailed ? "error" : "finished";

  function UpdateFromAPI() {
    if (updating) {
      console.log("UpdateFromAPI skipped because updating is true");
      return;
    }
    updating = true;
    console.log("Fetching data from API:", APIpath); // Debugging log
    fetch(CalculateAPIPath(APIpath))
      .then((res) => {
        if (!res.ok) {
          throw new Error("HTTP " + res.status);
        }
        return res.json();
      })
      .then((data) => {
        console.log("API response:", data);
        if (data.data && data.data.length > 0) {
          console.log("Calling transform function:", transform);
          rows = [...transform(data.data)]; // Use the transform function passed as a prop
        } else {
          console.warn("API returned empty data, keeping previous rows.");
        }
        console.log("Updated rows in APITable:", rows);
        status = data.status;
        fetchFailed = false;
        updating = false;
      })
      .catch((err) => {
        console.error("Error fetching data:", err);
        fetchFailed = true;
        status = "";
        updating = false;
      }).finally(() => {
        updating = false; // Always reset updating flag
      });
  }

  function safeLength(obj) {
    return obj ? Object.keys(obj).length : 0;
  }

  UpdateFromAPI();
  setInterval(() => {
    UpdateFromAPI();
  }, updateTimeSeconds * 1000);



</script>

<main>
  {#if totalName !== ""}
    <p>
      {totalName}
      {safeLength(rows)}
    </p>
  {/if}
  <p>
    <InlineLoading status={statusIndicator} description="Update status" />
  </p>
  <p>
    Message: {fetchFailed ? "Update failed — showing the last known data. Check that the daemon is reachable, then reload." : status}
  </p>
  <p>
    <DataTable sortable {headers} {rows} rowKey="name" />
  </p></main>