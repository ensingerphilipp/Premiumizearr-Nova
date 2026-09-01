<script>
  import APITable from "../components/APITable.svelte";
  import { Row, Column, Button } from "carbon-components-svelte";
  import { CalculateAPIPath } from "../Utilities/web_root";

  let dlSpeed = 0;
  let pollingBlackhole = false;
  let pollBlackholeMessage = "";

  async function pollBlackholeNow() {
    pollingBlackhole = true;
    pollBlackholeMessage = "";

    try {
      const response = await fetch(CalculateAPIPath("api/blackhole/poll"), {
        method: "POST",
      });
      if (!response.ok) {
        throw new Error((await response.text()).trim() || response.statusText);
      }

      const data = await response.json();
      pollBlackholeMessage = `Queued ${data.queued} file${data.queued === 1 ? "" : "s"}.`;
    } catch (error) {
      pollBlackholeMessage = `Poll failed: ${error.message || error}`;
    } finally {
      pollingBlackhole = false;
    }
  }

  function parseDLSpeedFromMessage(m) {
    if (m == "Loading..." || m == undefined) return 0;
    if (m == "too many missing articles") return 0;
    
    let speed = m.split(" ")[0];
    speed = speed.replace(",", "");
    let unit = m.split(" ")[1];
    if (Number.isNaN(speed)) {
      console.log("Speed is not a number: ", speed);
      console.log("Message: ", message);
      return 0;
    }
    if (unit === undefined || unit === null || unit == "") {
      console.log("Unit undefined in : " + m);
      return 0;
    } else {
      try {
        unit = unit.toUpperCase();
      } catch (error) {
        return 0;
      }
      unit = unit.replace("/", "");
      unit = unit.substring(0, 2);
      switch (unit) {
        case "KB":
          return Number(speed) * 1024;
        case "MB":
          return Number(speed) * 1024 * 1024;
        case "GB":
          return Number(speed) * 1024 * 1024 * 1024;
        default:
          console.log("Unknown unit: " + unit + " in message '" + m + "'");
          return 0;
      }
    }
  }

  function HumanReadableSpeed(bytes) {
    if (bytes < 1024) {
      return bytes + " B/s";
    } else if (bytes < 1024 * 1024) {
      return (bytes / 1024).toFixed(2) + " KB/s";
    } else if (bytes < 1024 * 1024 * 1024) {
      return (bytes / 1024 / 1024).toFixed(2) + " MB/s";
    } else {
      return (bytes / 1024 / 1024 / 1024).toFixed(2) + " GB/s";
    }
  }

  // Helper function to get the ordinal suffix for a day number
  function getOrdinalSuffix(day) {
    if (day > 3 && day < 21) return 'th'; // Covers 11th, 12th, 13th
    switch (day % 10) {
      case 1:  return "st";
      case 2:  return "nd";
      case 3:  return "rd";
      default: return "th";
    }
  }

  function dataToRowsDownload(data) {
    console.log("Transforming data:", data); // Log the input data
    if (!data) return [];

    // Filter out rows with "0% Complete (0 B)" or similar variations
    const filteredData = data.filter(d => {
      // Check if d.progress is actually a string before calling .includes
      // Also handle potential null/undefined values for d or d.progress
      const progressString = d?.progress?.toString() || "";
      console.log("Progress field value:", progressString); // Log progress field
      return !progressString.includes("(0 B)");
    });

    console.log("Filtered data:", filteredData); // Log data after filtering

    const transformed = filteredData.map((d, index) => {
      let readableAdded = 'Invalid Date'; // Default value

      // Check if d.added is a valid number before processing
      if (typeof d.added === 'number' && !isNaN(d.added)) {
        try {
          // 1. Create a Date object (multiply Unix seconds by 1000 for milliseconds)
          const dateObject = new Date(d.added * 1000);

          // Check if the dateObject is valid after creation
          if (isNaN(dateObject.getTime())) {
            console.error("Invalid date created for added:", d.added);
            // Keep readableAdded as 'Invalid Date'
          } else {
              // 2. Format Time (HH:MM) - using 'en-GB' often defaults to 24-hour format
              const timeFormatter = new Intl.DateTimeFormat('en-GB', {
                hour: '2-digit',
                minute: '2-digit',
                hour12: false // Explicitly set 24-hour format
              });
              const timeString = timeFormatter.format(dateObject); // e.g., "15:37"

              // 3. Format Date Part (Mon Day) - using 'en-US' for common abbreviations
              const dateFormatter = new Intl.DateTimeFormat('en-US', {
                month: 'short', // e.g., "Apr"
                day: 'numeric'  // e.g., "4"
              });
              const datePartString = dateFormatter.format(dateObject); // e.g., "Apr 4"

              // 4. Get the day number and calculate the suffix
              const day = dateObject.getDate(); // Get day of the month (1-31)
              const suffix = getOrdinalSuffix(day);

              // 5. Combine the parts into the desired format
              readableAdded = `${timeString}, ${datePartString}${suffix}`; // e.g., "15:37, Apr 4th"
          }

        } catch (error) {
          console.error("Error formatting date for added value:", d.added, error);
          // Keep readableAdded as 'Invalid Date' on error
        }
      } else {
        console.warn("Invalid or missing 'added' timestamp for item:", d);
      }


      return {
        // Using nullish coalescing (??) is generally safer than || for IDs
        // as it handles 0 or empty string correctly if they are valid IDs.
        id: d.id ?? index,
        added: readableAdded, // Use the newly formatted string
        name: d.name,
        progress: d.progress,
        speed: d.speed,
      };
    });

    console.log("Transformed Info rows:", transformed); // Log the final transformed rows
    return transformed;
  }

  function dataToRows(data) {
      if (!data) return [];

      let dlSpeed = 0;

      return data.map(d => {
          let speed = parseDLSpeedFromMessage(d.message);
          if (!Number.isNaN(speed)) dlSpeed += speed;

          return {
              // Using nullish coalescing (??) is generally safer than || for IDs
              // as it handles 0 or empty string correctly if they are valid IDs.
              id: d.id ?? index,            
              name: d.name,
              status: d.status,
              progress: (d.progress * 100).toFixed(0) + "%",
              message: d.message,
          };
      });
  }

</script>

<main>
    <Row>
      <Column md={4} >
        <h3>Blackhole</h3>
        <Button
          size="sm"
          kind="secondary"
          disabled={pollingBlackhole}
          on:click={pollBlackholeNow}
        >
          {pollingBlackhole ? "Polling..." : "Poll now"}
        </Button>
        {#if pollBlackholeMessage}
          <p>{pollBlackholeMessage}</p>
        {/if}
        <APITable
          headers={[
            { key: "id", value: "Pos" },
            { key: "name", value: "Name", sort: false },
          ]}
          APIpath="api/blackhole"
          zebra={true}
          totalName="In Queue: "
        />
      </Column>
      <Column md={4} >
        <h3>Downloads</h3>
        <APITable
          headers={[
            { key: "added", value: "Added" },
            { key: "name", value: "Name" },
            { key: "progress", value: "Progress" },
            { key: "speed", value: "Speed" },
          ]}
          updateTimeSeconds={2}
          APIpath="api/downloads"
          zebra={true}
          totalName="Downloading: "
          transform={dataToRowsDownload}
        />
      </Column>
    </Row>
    <Row>
      <Column>
        <h3>Transfers</h3>
        <p>Download Speed: {HumanReadableSpeed(dlSpeed)}</p>
        <APITable
          headers={[
            { key: "name", value: "Name" },
            { key: "status", value: "Status" },
            { key: "progress", value: "Progress" },
            { key: "message", value: "Message", sort: false },
          ]}
          APIpath="api/transfers"
          zebra={true}
          transform={dataToRows}
        />
      </Column>
    </Row>
</main>
