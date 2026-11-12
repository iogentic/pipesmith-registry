// Parts the task screens S38, S39 and S40 share: marks for state and result, the names of the
// schedule types, local times in the form fields, and the poller that follows a running task.

import { h } from "../../dom.js";
import { mark } from "../../ui/notice.js";
import { titleCase } from "../../format.js";

export const SCHEDULES = [
  { value: "manual", name: "Manual", label: "Manual: only when started here" },
  { value: "once", name: "Once", label: "Once" },
  { value: "hourly", name: "Hourly", label: "Every hour" },
  { value: "daily", name: "Daily", label: "Every day" },
  { value: "weekly", name: "Weekly", label: "On days of the week" },
  { value: "monthly", name: "Monthly", label: "On days of the month" },
  { value: "cron", name: "Cron", label: "Cron expression" }
];

// The schedule types whose settings carry startAt.
export const NEEDS_START = ["once", "hourly", "daily", "weekly", "monthly"];

export const WEEK_DAYS = [
  { id: "MON", label: "Monday" },
  { id: "TUE", label: "Tuesday" },
  { id: "WED", label: "Wednesday" },
  { id: "THU", label: "Thursday" },
  { id: "FRI", label: "Friday" },
  { id: "SAT", label: "Saturday" },
  { id: "SUN", label: "Sunday" }
];

// A type id such as repository.rebuild-index, allowed to wrap after each dot.
export function typeIdView(id) {
  const parts = String(id || "").split(".");
  return h("span", { class: "mono small task-type" }, parts.map((part, i) => (i < parts.length - 1 ? [`${part}.`, h("wbr")] : part)));
}

export function scheduleName(type) {
  const found = SCHEDULES.find((s) => s.value === type);
  return found ? found.name : titleCase(String(type || ""));
}

// States the contract names get their own mark; any other value is shown as it comes.
export function stateMark(state) {
  if (state === "RUNNING") return mark("busy", "Running");
  if (state === "WAITING") return mark("info", "Waiting");
  if (state === "CANCELED") return mark("warn", "Canceled");
  return mark("", state ? titleCase(String(state).toLowerCase()) : "Unknown");
}

export function resultMark(result) {
  if (result === "OK") return mark("ok", "OK");
  if (result === "FAILED") return mark("error", "Failed");
  if (result === "CANCELED") return mark("warn", "Canceled");
  if (result === null || result === undefined || result === "") return mark("off", "Not run yet");
  return mark("", titleCase(String(result).toLowerCase()));
}

// "YYYY-MM-DD HH:MM" in the time zone of the browser as a Date, or null.
export function parseLocal(text) {
  const m = /^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2})$/.exec(String(text || "").trim());
  if (!m) return null;
  const [y, mo, d, hh, mm] = m.slice(1).map(Number);
  const date = new Date(y, mo - 1, d, hh, mm, 0, 0);
  if (date.getFullYear() !== y || date.getMonth() !== mo - 1 || date.getDate() !== d || date.getHours() !== hh || date.getMinutes() !== mm) return null;
  return date;
}

// The offset of the browser's time zone, for example UTC+01:00.
export function zoneText() {
  const offset = -new Date().getTimezoneOffset();
  const sign = offset < 0 ? "-" : "+";
  const abs = Math.abs(offset);
  return `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, "0")}:${String(abs % 60).padStart(2, "0")}`;
}

// Calls load() again while it answers true: every second for 30 seconds after kick(), every 10
// seconds after that. stop() ends it for good, for the dispose of a page.
export function poller(load) {
  let timer = null;
  let fastUntil = 0;
  let stopped = false;
  const schedule = (active) => {
    clearTimeout(timer);
    timer = null;
    if (stopped || !active) return;
    const delay = Date.now() < fastUntil ? 1000 : 10000;
    timer = setTimeout(async () => {
      timer = null;
      let again = true;
      try {
        again = await load();
      } catch (e) {
        again = true;
      }
      schedule(again);
    }, delay);
  };
  return {
    kick() {
      fastUntil = Date.now() + 30000;
    },
    update: schedule,
    stop() {
      stopped = true;
      clearTimeout(timer);
      timer = null;
    }
  };
}
