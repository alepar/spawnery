import { useEffect, useState } from "react";
import { listProfiles, type ProfileSummary } from "@/api/profiles";

/**
 * ProfileSelect — a lightweight <select> populated from listProfiles.
 * Default option is "None" (empty string value).
 */
export function ProfileSelect({
  value,
  onChange,
}: {
  value: string;
  onChange: (profileId: string) => void;
}) {
  const [profiles, setProfiles] = useState<ProfileSummary[]>([]);

  useEffect(() => {
    listProfiles().then(setProfiles).catch(() => {});
  }, []);

  return (
    <select
      data-testid="profile-select"
      aria-label="Profile"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      <option value="">None</option>
      {profiles.map((p) => (
        <option key={p.profileId} value={p.profileId}>
          {p.name}
        </option>
      ))}
    </select>
  );
}
